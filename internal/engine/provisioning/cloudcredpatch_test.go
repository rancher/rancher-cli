package provisioning

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	ccpID = "cc-abcde"
	// ccpAnnotation is the value of a stored annotation whose key has no
	// cattle.io/ and ccpStoredData the decoded value of a stored data key; no
	// output may show either.
	ccpAnnotation = "planted-annotation-0001"
	ccpStoredData = "planted-stored-data-0001"
)

// ccpStored is the Secret a credential of driver is stored as.
func ccpStored(driver, resourceVersion string) *unstructured.Unstructured {
	obj := secretObj(ccpID, map[string]string{
		"field.cattle.io/name":          "prod",
		"field.cattle.io/description":   "old description",
		"field.cattle.io/creatorId":     "u-creator",
		"provisioning.cattle.io/driver": "aws",
		"e2e/keep":                      ccpAnnotation,
	}, map[string]any{driver + "credentialConfig-stored": base64.StdEncoding.EncodeToString([]byte(ccpStoredData))})
	obj.SetResourceVersion(resourceVersion)
	obj.SetUID("uid-1")
	return obj
}

// ccpRig is a patch command over a rig that has one stored credential.
type ccpRig struct {
	*ccwRig
	patch   *patchCloudCredentialCmd
	mu      sync.Mutex
	current *unstructured.Unstructured
}

func newCCPRig(base *ccwRig, driver string) *ccpRig {
	r := &ccpRig{ccwRig: base, current: ccpStored(driver, "7")}
	r.client.secrets = listOf(r.current.DeepCopy())
	r.client.getSecret = func(name string) (*unstructured.Unstructured, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.current == nil || name != ccpID {
			return nil, clierr.New(clierr.CodeNotFound, "secret not found")
		}
		return r.current.DeepCopy(), nil
	}
	r.patch = NewPatchCloudCredential(fakeFactory{c: r.client}, r.norman, r.files).(*patchCloudCredentialCmd)
	return r
}

// change replaces the Secret the next read returns.
func (r *ccpRig) change(obj *unstructured.Unstructured) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = obj
}

func (r *ccpRig) plan(t *testing.T, in ccwInput) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	return r.patch.Plan(t.Context(), in)
}

// run sends the command through the pipeline in JSON and returns what it
// wrote to the output, everything it wrote, and its error.
func (r *ccpRig) run(t *testing.T, in ccwInput, format string, dryRun, yes bool) (written, stdout string, ce *clierr.Error) {
	t.Helper()
	var out, errOut bytes.Buffer
	_, ce = runtime.Run(t.Context(), r.patch, in, runtime.IO{Out: &out, Err: &errOut, Format: format, DryRun: dryRun, Apply: !dryRun, Yes: yes}, safety.Binding{})
	written = out.String() + errOut.String()
	if ce != nil {
		raw, _ := json.Marshal(ce)
		written += ce.Error() + ce.Message + ce.Hint + string(raw)
	}
	return written, out.String(), ce
}

func (r *ccpRig) sent(t *testing.T) map[string]any {
	t.Helper()
	r.norman.mu.Lock()
	defer r.norman.mu.Unlock()
	require.Len(t, r.norman.updateBodies, 1)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.norman.updateBodies[0]), &out))
	return out
}

func ccpInputOf() ccwInput { return ccwInput{args: []string{ccpID}} }

// TestCloudCredentialPatchEffect asserts the diff and the warnings of a
// preview: the stored name and description as From, public values as typed,
// secret values hidden, no From for a config field (marked unread), and the
// warnings in order.
func TestCloudCredentialPatchEffect(t *testing.T) {
	t.Parallel()
	r := newCCPRig(newCCWRig(ccwFilesFor(ccwSecret+"\n")), "amazonec2")
	in := ccpInputOf()
	in.display, in.description = new("renamed"), new("new")
	in.set = []string{"accessKey=AKIAFAKEEXAMPLE"}
	in.fromFile = []string{"secretKey=secret.txt"}
	eff, _, err := r.plan(t, in)
	require.NoError(t, err)
	assert.Equal(t, []safety.DiffEntry{
		{Path: "/name", From: "prod", To: "renamed"},
		{Path: "/description", From: "old description", To: "new"},
		{Path: "/amazonec2credentialConfig/accessKey", To: "AKIAFAKEEXAMPLE", FromUnread: true},
		{Path: "/amazonec2credentialConfig/secretKey", To: engine.Redacted, FromUnread: true},
	}, eff.Diff)
	assert.Equal(t, []string{
		"previewed from server reads: Rancher checks the request only when it is sent; it does not check the values against the provider",
		"fields not given keep their values",
		"--apply reads the cloud credential again and stops if it changed; a change after that read is not detected",
	}, eff.Warnings)
	assert.True(t, eff.Destructive, "a secret field in --from-file replaces a secret value")
	assert.False(t, eff.Reversible)
}

// TestCloudCredentialPatchDiffLeavesOutFromOfUnreadValues asserts, for every
// driver of the table, that the config entries of the preview's diff have no
// `from` key in the JSON envelope (the CLI does not read the stored values),
// and that the entries of the name and the description keep theirs, null when
// the credential has no description.
func TestCloudCredentialPatchDiffLeavesOutFromOfUnreadValues(t *testing.T) {
	t.Parallel()
	type entry map[string]any
	diffOf := func(t *testing.T, stdout string) map[string]entry {
		t.Helper()
		var envelope struct {
			Data struct {
				Diff []entry `json:"diff"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(stdout), &envelope), stdout)
		out := map[string]entry{}
		for _, e := range envelope.Data.Diff {
			out[e["path"].(string)] = e
		}
		require.Len(t, out, len(envelope.Data.Diff), "every path appears once")
		return out
	}
	for name, run := range ccsRuns() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			files := maps.Clone(run.files)
			fromFile := slices.Clone(run.in.fromFile)
			if name == "s3" {
				// ccsRuns leaves out the public multi-line field, which a run
				// gives through --from-file; its value is the preview's to
				// show, so it is not a secret value to look for.
				files["defaultEndpointCA.txt"] = "fake-public-line-one\nfake-public-line-two\n"
				fromFile = append(fromFile, "defaultEndpointCA=defaultEndpointCA.txt")
			}
			r := newCCPRig(ccsRig(files), name)
			in := ccpInputOf()
			in.set, in.fromFile = run.in.set, fromFile
			in.display, in.description = new("renamed"), new("new")
			_, stdout, ce := r.run(t, in, "json", true, false)
			require.Nil(t, ce)
			diff := diffOf(t, stdout)
			require.Len(t, diff, 2+len(in.set)+len(in.fromFile))
			assert.Equal(t, entry{"path": "/name", "from": "prod", "to": "renamed"}, diff["/name"])
			assert.Equal(t, entry{"path": "/description", "from": "old description", "to": "new"}, diff["/description"])
			configKey := "/" + name + "credentialConfig/"
			var config int
			for path, e := range diff {
				if !strings.HasPrefix(path, configKey) {
					continue
				}
				config++
				assert.NotContains(t, e, "from", path)
				assert.Contains(t, e, "to", path)
			}
			assert.Equal(t, len(in.set)+len(in.fromFile), config, "every field of the command line has an entry")
		})
	}
	t.Run("a credential with no description", func(t *testing.T) {
		t.Parallel()
		r := newCCPRig(newCCWRig(nil), "amazonec2")
		stored := ccpStored("amazonec2", "7")
		annotations := stored.GetAnnotations()
		delete(annotations, "field.cattle.io/description")
		stored.SetAnnotations(annotations)
		r.change(stored)
		r.client.secrets = listOf(stored.DeepCopy())
		in := ccpInputOf()
		in.description = new("new")
		_, stdout, ce := r.run(t, in, "json", true, false)
		require.Nil(t, ce)
		diff := diffOf(t, stdout)
		require.Contains(t, diff["/description"], "from", "an absent stored description is null, not left out")
		assert.Nil(t, diff["/description"]["from"])
	})
}

// TestCloudCredentialPatchWarnings asserts the access review and the display
// name List add their warnings after the fixed ones, and a trimmed
// multi-line value is reported.
func TestCloudCredentialPatchWarnings(t *testing.T) {
	t.Parallel()
	t.Run("the review is not made", func(t *testing.T) {
		t.Parallel()
		r := newCCPRig(newCCWRig(nil), "amazonec2")
		r.client.review = func() (engine.ReviewResponse, error) { return engine.ReviewResponse{}, assert.AnError }
		in := ccpInputOf()
		in.description = new("new")
		eff, _, err := r.plan(t, in)
		require.NoError(t, err)
		assert.Equal(t, "whether you have permission to update the cloud credential was not checked; Rancher checks it when the request is sent", eff.Warnings[len(eff.Warnings)-1])
	})
	t.Run("the display name List is refused", func(t *testing.T) {
		t.Parallel()
		r := newCCPRig(newCCWRig(nil), "amazonec2")
		r.client.secretsErr = clierr.New(clierr.CodeRBAC, "forbidden")
		in := ccpInputOf()
		in.display = new("renamed")
		eff, _, err := r.plan(t, in)
		require.NoError(t, err)
		assert.Contains(t, strings.Join(eff.Warnings, "\n"), "the display name was not checked against the existing cloud credentials because you cannot list them")
	})
	t.Run("a multi-line value is trimmed", func(t *testing.T) {
		t.Parallel()
		r := newCCPRig(newCCWRig(map[string]string{"ca.pem": "line one\nline two\n\n"}), "s3")
		in := ccpInputOf()
		in.fromFile = []string{"defaultEndpointCA=ca.pem"}
		eff, _, err := r.plan(t, in)
		require.NoError(t, err)
		assert.True(t, slices.ContainsFunc(eff.Warnings, func(w string) bool { return strings.Contains(w, "was trimmed") }), eff.Warnings)
		assert.False(t, eff.Destructive, "a multi-line public field replaces no secret value")
	})
}

// TestCloudCredentialPatchPlanRefusals asserts each refusal of the plan: its
// code and text, and that nothing is sent. An empty --display-name or
// --description is the pipeline's refusal of every empty flag value.
func TestCloudCredentialPatchPlanRefusals(t *testing.T) {
	t.Parallel()
	type row struct {
		name   string
		driver string
		in     ccwInput
		prep   func(*ccpRig)
		code   string
		want   string
	}
	patchIn := func(mutate func(*ccwInput)) ccwInput {
		in := ccpInputOf()
		mutate(&in)
		return in
	}
	rows := []row{
		{name: "no field flag", driver: "amazonec2", in: ccpInputOf(), code: clierr.CodeValidation, want: "give at least one of --set, --from-file, --display-name or --description"},
		{name: "an empty display name", driver: "amazonec2", in: patchIn(func(in *ccwInput) { in.display = new("") }), code: clierr.CodeValidation,
			want: "--display-name is empty; pass a value or omit --display-name"},
		{name: "an empty description", driver: "amazonec2", in: patchIn(func(in *ccwInput) { in.description = new("") }), code: clierr.CodeValidation,
			want: "--description is empty; pass a value or omit --description"},
		{name: "a credential that is not found", driver: "amazonec2", in: ccwInput{args: []string{"nope"}, description: new("x")}, code: clierr.CodeNotFound,
			want: "cloud credential not found: nope"},
		{name: "a Secret with no driver config", driver: "amazonec2", in: patchIn(func(in *ccwInput) { in.description = new("x") }), code: clierr.CodeValidation,
			want: "the cloud credential cc-abcde has no fields of a node driver; the CLI cannot tell which driver it belongs to",
			prep: func(r *ccpRig) {
				obj := secretObj(ccpID, nil, map[string]any{"other": "eA=="})
				r.client.secrets = listOf(obj)
			}},
		{name: "a driver the server does not have", driver: "vanished", in: patchIn(func(in *ccwInput) { in.description = new("x") }), code: clierr.CodeValidation,
			want: "the node driver vanished of cloud credential cc-abcde is not on this server; Rancher cannot change a credential of a driver it does not have"},
		{name: "an inactive driver", driver: "linode", in: patchIn(func(in *ccwInput) { in.description = new("x") }), code: clierr.CodeValidation,
			want: `node driver "linode" is not active; an administrator activates it`},
		{name: "alibaba while its operator is inactive", driver: "alibaba", in: patchIn(func(in *ccwInput) { in.description = new("x") }), code: clierr.CodeValidation,
			want: "the alibaba operator is not active"},
		{name: "a field the driver does not have", driver: "amazonec2", in: patchIn(func(in *ccwInput) { in.set = []string{"nope=1"} }), code: clierr.CodeValidation,
			want: "is not a field of amazonec2"},
		{name: "a secret field in --set", driver: "amazonec2", in: patchIn(func(in *ccwInput) { in.set = []string{"secretKey=" + ccwSecret} }), code: clierr.CodeValidation,
			want: "is a secret field and cannot be given with --set"},
		{name: "a display name another credential has", driver: "amazonec2", in: patchIn(func(in *ccwInput) { in.display = new("taken") }), code: clierr.CodeValidation,
			want: "a cloud credential with the display name \"taken\" exists (id cc-other)",
			prep: func(r *ccpRig) {
				r.client.secrets = listOf(ccpStored("amazonec2", "7"),
					secretObj("cc-other", map[string]string{"field.cattle.io/name": "taken"}, map[string]any{"amazonec2credentialConfig-accessKey": "x"}))
			}},
		{name: "an access review that denies", driver: "amazonec2", in: patchIn(func(in *ccwInput) { in.description = new("x") }), code: clierr.CodeRBAC,
			want: "you do not have permission to update cloud credential cc-abcde: Rancher gives that permission to the user who created it",
			prep: func(r *ccpRig) {
				r.client.review = func() (engine.ReviewResponse, error) { return reviewAnswer(false), nil }
			}},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := newCCWRig(nil)
			if tc.driver == "alibaba" {
				base.client.setting = `[{"name":"alibaba","active":false}]`
			}
			r := newCCPRig(base, tc.driver)
			if tc.prep != nil {
				tc.prep(r)
			}
			_, _, ce := r.run(t, tc.in, "json", true, false)
			require.NotNil(t, ce)
			assert.Equal(t, tc.code, ce.Code, ce.Message)
			assert.Contains(t, ce.Message, tc.want)
			assert.Empty(t, r.norman.updates, "a refusal sends no request")
		})
	}
}

// TestCloudCredentialPatchDisplayNameOfItself asserts a patch may give the
// credential its own display name: it is not another credential.
func TestCloudCredentialPatchDisplayNameOfItself(t *testing.T) {
	t.Parallel()
	r := newCCPRig(newCCWRig(nil), "amazonec2")
	in := ccpInputOf()
	in.display = new("prod")
	_, _, err := r.plan(t, in)
	require.NoError(t, err)
}

// TestCloudCredentialPatchCommandLineBeforeAnyRead asserts the shapes of the
// command line that no server read can change are refused before a request
// and before stdin or a file is read.
func TestCloudCredentialPatchCommandLineBeforeAnyRead(t *testing.T) {
	t.Parallel()
	r := newCCPRig(newCCWRig(ccwFilesFor(ccwSecret)), "amazonec2")
	for _, in := range []ccwInput{
		ccpInputOf(),
		{args: []string{ccpID}, fromFile: []string{"secretKey=-", "accessKey=-"}},
		{args: []string{ccpID}, fromFile: []string{"secretKey=-"}, terminal: true},
		{args: []string{ccpID}, set: []string{"noequals"}, fromFile: []string{"secretKey=secret.txt"}},
	} {
		_, _, err := r.plan(t, in)
		require.Error(t, err)
		assert.Equal(t, clierr.CodeValidation, clierr.From(err).Code)
	}
	assert.Empty(t, r.files.reads, "no file or stdin was read")
	assert.Zero(t, r.client.gets+r.client.lists, "no server read")
}

// TestCloudCredentialPatchGates asserts the gates of a patch: --apply always,
// --yes when a secret field is replaced, and not for a public field, the
// description, or a multi-line public field of s3.
func TestCloudCredentialPatchGates(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "dependsOnInput", NewPatchCloudCredential(fakeFactory{}, &ccwNorman{}, &ccwFiles{}).Describe().Destructive)
	for _, tc := range []struct {
		name   string
		driver string
		in     func() ccwInput
		files  map[string]string
		yes    bool
		want   string
	}{
		{"a secret value without --yes", "amazonec2", func() ccwInput {
			in := ccpInputOf()
			in.fromFile = []string{"secretKey=secret.txt"}
			return in
		}, ccwFilesFor(ccwSecret), false, clierr.CodeConfirmationRequired},
		{"a secret value with --yes", "amazonec2", func() ccwInput {
			in := ccpInputOf()
			in.fromFile = []string{"secretKey=secret.txt"}
			return in
		}, ccwFilesFor(ccwSecret), true, ""},
		{"a public value", "amazonec2", func() ccwInput {
			in := ccpInputOf()
			in.set = []string{"accessKey=AKIAFAKEEXAMPLE"}
			return in
		}, nil, false, ""},
		{"the description", "amazonec2", func() ccwInput {
			in := ccpInputOf()
			in.description = new("new")
			return in
		}, nil, false, ""},
		{"a multi-line public field of s3", "s3", func() ccwInput {
			in := ccpInputOf()
			in.fromFile = []string{"defaultEndpointCA=secret.txt"}
			return in
		}, ccwFilesFor("line one\nline two\n"), false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCCPRig(newCCWRig(tc.files), tc.driver)
			_, _, ce := r.run(t, tc.in(), "json", false, tc.yes)
			if tc.want == "" {
				require.Nil(t, ce)
				assert.Len(t, r.norman.updates, 1)
				return
			}
			require.NotNil(t, ce)
			assert.Equal(t, tc.want, ce.Code)
			assert.Empty(t, r.norman.updates)
		})
	}
}

// TestCloudCredentialPatchDryRunSendsNoWrite asserts a preview sends no write
// on any path, a refused display name List and a failed access review
// included.
func TestCloudCredentialPatchDryRunSendsNoWrite(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*ccpRig){
		"plain": func(*ccpRig) {},
		"review request failed": func(r *ccpRig) {
			r.client.review = func() (engine.ReviewResponse, error) { return engine.ReviewResponse{}, assert.AnError }
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newCCPRig(newCCWRig(ccwFilesFor(ccwSecret)), "amazonec2")
			mutate(r)
			in := ccpInputOf()
			in.fromFile = []string{"secretKey=secret.txt"}
			in.description = new("new")
			_, _, ce := r.run(t, in, "json", true, false)
			require.Nil(t, ce)
			assert.Empty(t, r.norman.updates)
			assert.Empty(t, r.norman.deletes)
		})
	}
}

// TestCloudCredentialUpdateBody asserts each PUT body has only the fields the
// patch gives: the config key is always there, empty when no config field is
// given, name and description only with their flags, annotations only with
// either flag and only the stored ones whose key has no cattle.io/, and a
// multi-line value keeps the line endings the reader keeps.
func TestCloudCredentialUpdateBody(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		driver string
		files  map[string]string
		in     func() ccwInput
		check  func(t *testing.T, sent map[string]any)
	}{
		{"the description alone", "amazonec2", nil, func() ccwInput {
			in := ccpInputOf()
			in.description = new("new")
			return in
		}, func(t *testing.T, sent map[string]any) {
			t.Helper()
			assert.Equal(t, map[string]any{"amazonec2credentialConfig": map[string]any{}, "description": "new", "annotations": map[string]any{"e2e/keep": ccpAnnotation}}, sent)
		}},
		{"the display name alone", "amazonec2", nil, func() ccwInput {
			in := ccpInputOf()
			in.display = new("renamed")
			return in
		}, func(t *testing.T, sent map[string]any) {
			t.Helper()
			assert.Equal(t, map[string]any{"amazonec2credentialConfig": map[string]any{}, "name": "renamed", "annotations": map[string]any{"e2e/keep": ccpAnnotation}}, sent)
		}},
		{"a public field alone", "amazonec2", nil, func() ccwInput {
			in := ccpInputOf()
			in.set = []string{"accessKey=AKIAFAKEEXAMPLE"}
			return in
		}, func(t *testing.T, sent map[string]any) {
			t.Helper()
			assert.Equal(t, map[string]any{"amazonec2credentialConfig": map[string]any{"accessKey": "AKIAFAKEEXAMPLE"}}, sent, "no annotations without a name or a description")
		}},
		{"a secret field alone", "amazonec2", ccwFilesFor(ccwSecret + "\n"), func() ccwInput {
			in := ccpInputOf()
			in.fromFile = []string{"secretKey=secret.txt"}
			return in
		}, func(t *testing.T, sent map[string]any) {
			t.Helper()
			assert.Equal(t, map[string]any{"amazonec2credentialConfig": map[string]any{"secretKey": ccwSecret}}, sent)
		}},
		{"a multi-line field keeps its line endings", "s3", map[string]string{"ca.pem": "line one\r\nline two\r\n"}, func() ccwInput {
			in := ccpInputOf()
			in.fromFile = []string{"defaultEndpointCA=ca.pem"}
			return in
		}, func(t *testing.T, sent map[string]any) {
			t.Helper()
			assert.Equal(t, map[string]any{"s3credentialConfig": map[string]any{"defaultEndpointCA": "line one\r\nline two"}}, sent)
		}},
		{"every flag", "amazonec2", ccwFilesFor(ccwSecret), func() ccwInput {
			in := ccpInputOf()
			in.display, in.description = new("renamed"), new("new")
			in.set = []string{"accessKey=AKIAFAKEEXAMPLE"}
			in.fromFile = []string{"secretKey=secret.txt"}
			return in
		}, func(t *testing.T, sent map[string]any) {
			t.Helper()
			assert.Equal(t, map[string]any{
				"amazonec2credentialConfig": map[string]any{"accessKey": "AKIAFAKEEXAMPLE", "secretKey": ccwSecret},
				"name":                      "renamed", "description": "new", "annotations": map[string]any{"e2e/keep": ccpAnnotation},
			}, sent)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCCPRig(newCCWRig(tc.files), tc.driver)
			_, _, ce := r.run(t, tc.in(), "json", false, true)
			require.Nil(t, ce)
			tc.check(t, r.sent(t))
			assert.Equal(t, []string{"cattle-global-data:" + ccpID}, r.norman.updates)
		})
	}
}

// TestCloudCredentialReReadRefusesAChange asserts a patch reads the Secret
// again before its one request: a different resourceVersion ends in CONFLICT,
// retryable, with nothing sent; a Secret that is gone is NOT_FOUND with the hint
// to list the credentials; an unchanged one sends the one PUT.
func TestCloudCredentialReReadRefusesAChange(t *testing.T) {
	t.Parallel()
	in := func() ccwInput {
		in := ccpInputOf()
		in.description = new("new")
		return in
	}
	t.Run("the preview carries the resourceVersion and no conflict detection", func(t *testing.T) {
		t.Parallel()
		r := newCCPRig(newCCWRig(nil), "amazonec2")
		eff, _, err := r.plan(t, in())
		require.NoError(t, err)
		assert.Equal(t, "7", eff.ResourceVersion)
		assert.False(t, eff.ConflictDetection)
	})
	t.Run("a different resourceVersion", func(t *testing.T) {
		t.Parallel()
		r := newCCPRig(newCCWRig(nil), "amazonec2")
		_, exec, err := r.plan(t, in())
		require.NoError(t, err)
		r.change(ccpStored("amazonec2", "8"))
		_, err = exec(t.Context())
		ce := clierr.From(err)
		assert.Equal(t, clierr.CodeConflict, ce.Code)
		assert.True(t, ce.Retryable)
		assert.Equal(t, "the cloud credential changed since the preview; try again", ce.Message)
		assert.Empty(t, r.norman.updates)
	})
	t.Run("a Secret that is gone", func(t *testing.T) {
		t.Parallel()
		r := newCCPRig(newCCWRig(nil), "amazonec2")
		_, exec, err := r.plan(t, in())
		require.NoError(t, err)
		r.change(nil)
		_, err = exec(t.Context())
		ce := clierr.From(err)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.Equal(t, "cloud credential not found: "+ccpID, ce.Message)
		assert.Contains(t, ce.Hint, "rancher get cloud-credential")
		assert.Empty(t, r.norman.updates)
	})
	t.Run("an unchanged Secret", func(t *testing.T) {
		t.Parallel()
		r := newCCPRig(newCCWRig(nil), "amazonec2")
		_, exec, err := r.plan(t, in())
		require.NoError(t, err)
		_, err = exec(t.Context())
		require.NoError(t, err)
		assert.Len(t, r.norman.updates, 1)
	})
}

// TestCloudCredentialPatchResultRow asserts the result is the credential's row
// from the Secret read again, with the new display name when one was given.
func TestCloudCredentialPatchResultRow(t *testing.T) {
	t.Parallel()
	r := newCCPRig(newCCWRig(nil), "amazonec2")
	in := ccpInputOf()
	in.display = new("renamed")
	_, stdout, ce := r.run(t, in, "json", false, false)
	require.Nil(t, ce)
	var env struct {
		Kind string           `json:"kind"`
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &env))
	assert.Equal(t, "CloudCredential", env.Kind)
	require.Len(t, env.Data, 1)
	assert.Equal(t, map[string]any{"id": "cattle-global-data:" + ccpID, "secretName": ccpID, "displayName": "renamed", "driver": "amazonec2"}, env.Data[0])
}

// TestCloudCredentialPatchResponseClasses asserts the code, the retry advice, the
// text and the hint of each response a PUT can get, with no value in any text.
func TestCloudCredentialPatchResponseClasses(t *testing.T) {
	t.Parallel()
	request := &clierr.Request{Server: "https://rancher.test"}
	refused := func(status int, body string) error {
		return &engine.ActionRefused{Err: clierr.FromResponse("PUT x on the Rancher server", http.MethodPut, status, nil, request), Body: []byte(body), Request: request}
	}
	unknown := func(status int, body string) error {
		return &engine.ActionOutcomeUnknown{Err: clierr.ResponseError(http.MethodPut, status, nil), Body: []byte(body)}
	}
	echo := `{"type":"error","code":"X","message":"bad value ` + ccwSecret + ` here"}`
	harvester := func(tail string) string {
		return `{"type":"error","code":"ServerError","message":"failed to process harvester cloud credential: ` + tail + `"}`
	}
	for _, tc := range []struct {
		name      string
		harvester bool
		err       error
		code      string
		retryable bool
		contains  string
		hint      string
	}{
		{"401", false, refused(http.StatusUnauthorized, echo), clierr.CodeAuth, false, "", ""},
		{"403", false, refused(http.StatusForbidden, echo), clierr.CodeRBAC, false, "bad value <hidden> here", ""},
		{"404", false, refused(http.StatusNotFound, echo), clierr.CodeNotFound, false, "bad value <hidden> here", "rancher get cloud-credential"},
		{"409", false, refused(http.StatusConflict, echo), clierr.CodeConflict, false, "bad value <hidden> here", ""},
		{"422", false, refused(http.StatusUnprocessableEntity, echo), clierr.CodeValidation, false, "bad value <hidden> here", ""},
		{"other 500", false, unknown(http.StatusInternalServerError, echo), clierr.CodeGeneric, false, "did not say whether Rancher changed the cloud credential", "rancher get cloud-credential 'cc-abcde'"},
		{"502", false, unknown(http.StatusBadGateway, ""), clierr.CodeGeneric, false, "did not say whether Rancher changed the cloud credential", ""},
		{"504", false, unknown(http.StatusGatewayTimeout, ""), clierr.CodeGeneric, false, "did not say whether Rancher changed the cloud credential", ""},
		{"a lost response", false, &engine.ActionOutcomeUnknown{Err: clierr.New(clierr.CodeGeneric, "the connection failed")}, clierr.CodeGeneric, false, "no response arrived", ""},
		{"harvester parser error", true, unknown(http.StatusInternalServerError, harvester("failed to get tokens from kubeconfig: yaml: line 3: "+ccwSecret)), clierr.CodeValidation, false, "Rancher could not read the tokens", ""},
		{"harvester token in use", true, unknown(http.StatusInternalServerError, harvester("token kubeconfig-user-x is already in use by secret")), clierr.CodeValidation, false,
			"Rancher refused the kubeconfig: token kubeconfig-user-x is already in use by secret; remove --from-file kubeconfigContent to keep the stored kubeconfig, or give a newly generated one", ""},
		{"harvester expired token", true, unknown(http.StatusInternalServerError, harvester("token kubeconfig-user-x is expired")), clierr.CodeValidation, false, "Rancher refused the kubeconfig: token kubeconfig-user-x is expired", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			driver, field := "amazonec2", "secretKey"
			if tc.harvester {
				driver, field = "harvester", "kubeconfigContent"
			}
			r := newCCPRig(newCCWRig(map[string]string{"secret.txt": ccwSecret, "kc.txt": ccsKubeconfig("fake-token-0001")}), driver)
			r.norman.updateErr = tc.err
			in := ccpInputOf()
			in.fromFile = []string{field + "=secret.txt"}
			if tc.harvester {
				in.fromFile = []string{field + "=kc.txt"}
			}
			_, stderr, ce := r.run(t, in, "json", false, true)
			require.NotNil(t, ce)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.retryable, ce.Retryable)
			assert.Contains(t, ce.Message, tc.contains)
			if tc.hint != "" {
				assert.Contains(t, ce.Hint+ce.Message, tc.hint)
			}
			assert.NotContains(t, ce.Message+ce.Hint+stderr, ccwSecret)
			assert.NotContains(t, ce.Message+ce.Hint+stderr, base64.StdEncoding.EncodeToString([]byte(ccwSecret)))
			assert.Len(t, r.norman.updates, 1)
		})
	}
}

// TestCloudCredentialPatchValueWhiteSpace asserts a patch reads values as a
// create does: a mark at the start and a final line ending are dropped, a
// multi-line value is trimmed with a warning, and a value with white space left
// is refused.
func TestCloudCredentialPatchValueWhiteSpace(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		file, want string
		warning    bool
	}{
		"one mark":                    {"\xef\xbb\xbf" + ccwSecret + "\r\n", ccwSecret, false},
		"two marks":                   {"\xef\xbb\xbf\xef\xbb\xbf" + ccwSecret + "\n", ccwSecret, false},
		"a mark after one byte stays": {"a\xef\xbb\xbf" + ccwSecret, "a\xef\xbb\xbf" + ccwSecret, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newCCPRig(newCCWRig(map[string]string{"secret.txt": tc.file}), "amazonec2")
			in := ccpInputOf()
			in.fromFile = []string{"secretKey=secret.txt"}
			_, _, ce := r.run(t, in, "json", false, true)
			require.Nil(t, ce)
			assert.Equal(t, tc.want, r.sent(t)["amazonec2credentialConfig"].(map[string]any)["secretKey"])
		})
	}
	for _, tc := range []struct{ name, value, want string }{
		{"a trailing space", "a ", "Rancher removes leading and trailing white space from accessKey; remove it from the value"},
		{"a leading no-break space", " a", "Rancher removes leading and trailing white space from accessKey; remove it from the value"},
		{"a line break inside", "a\nb", "the value of accessKey holds a line break"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCCPRig(newCCWRig(nil), "amazonec2")
			in := ccpInputOf()
			in.set = []string{"accessKey=" + tc.value}
			_, _, err := r.plan(t, in)
			require.ErrorContains(t, err, tc.want)
			assert.Empty(t, r.norman.updates)
		})
	}
}

// TestCloudCredentialPatchReusesCreateTexts asserts each case a create and a
// patch both refuse gives the same code and text through both, so a patch has no
// copy of a text with other words.
func TestCloudCredentialPatchReusesCreateTexts(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"secret.txt": ccwSecret + "\n",
		"lines.txt":  "line one\nline two\n",
		"padded.txt": ccwSecret + " \n",
		"utf16.txt":  "\xff\xfe" + "f\x00a\x00k\x00e\x00",
	}
	for _, tc := range []struct {
		name     string
		set      []string
		fromFile []string
	}{
		{"an argument with no equals sign", []string{"accessKey"}, []string{"secretKey=secret.txt"}},
		{"an empty field name", []string{"=x"}, []string{"secretKey=secret.txt"}},
		{"an empty value", []string{"accessKey="}, []string{"secretKey=secret.txt"}},
		{"stdin given twice", nil, []string{"secretKey=-", "accessKey=-"}},
		{"a field given twice", []string{"accessKey=a1b2c3d4"}, []string{"accessKey=secret.txt"}},
		{"an unknown field", []string{"nope=1"}, []string{"secretKey=secret.txt"}},
		{"a secret in --set", []string{"secretKey=" + ccwSecret}, nil},
		{"a public field in --from-file", nil, []string{"accessKey=secret.txt"}},
		{"an unreadable file", nil, []string{"secretKey=missing.txt"}},
		{"a UTF-16 value", nil, []string{"secretKey=utf16.txt"}},
		{"a line break", nil, []string{"secretKey=lines.txt"}},
		{"edge white space", nil, []string{"secretKey=padded.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			create := newCCWRig(files)
			set, fromFile := slices.Clone(tc.set), slices.Clone(tc.fromFile)
			if !slices.ContainsFunc(slices.Concat(set, fromFile), func(w string) bool { return strings.HasPrefix(w, "accessKey") }) {
				set = append(set, "accessKey=AKIAFAKEEXAMPLE")
			}
			if !slices.ContainsFunc(slices.Concat(set, fromFile), func(w string) bool { return strings.HasPrefix(w, "secretKey") }) {
				fromFile = append(fromFile, "secretKey=secret.txt")
			}
			_, _, createErr := create.plan(t, ccwInput{args: []string{"amazonec2", "n"}, set: set, fromFile: fromFile})
			patch := newCCPRig(newCCWRig(files), "amazonec2")
			in := ccpInputOf()
			in.set, in.fromFile = tc.set, tc.fromFile
			_, _, patchErr := patch.plan(t, in)
			require.Error(t, createErr)
			require.Error(t, patchErr)
			c, p := clierr.From(createErr), clierr.From(patchErr)
			assert.Equal(t, c.Code, p.Code)
			assert.Equal(t, c.Message, p.Message)
		})
	}
	t.Run("the trim warning", func(t *testing.T) {
		t.Parallel()
		multi := map[string]string{"ca.pem": "line one\nline two\n\n", "secret.txt": ccwSecret}
		create := newCCWRig(multi)
		eff, _, err := create.plan(t, ccwInput{args: []string{"s3", "n"}, set: []string{"accessKey=AKIAFAKEEXAMPLE"}, fromFile: []string{"secretKey=secret.txt", "defaultEndpointCA=ca.pem"}})
		require.NoError(t, err)
		patch := newCCPRig(newCCWRig(multi), "s3")
		in := ccpInputOf()
		in.fromFile = []string{"defaultEndpointCA=ca.pem"}
		peff, _, err := patch.plan(t, in)
		require.NoError(t, err)
		trim := func(w []string) []string {
			return slices.DeleteFunc(slices.Clone(w), func(s string) bool { return !strings.Contains(s, "was trimmed") })
		}
		require.NotEmpty(t, trim(eff.Warnings))
		assert.Equal(t, trim(eff.Warnings), trim(peff.Warnings))
	})
}

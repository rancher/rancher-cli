package user

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeUserClient serves both the users GVR and the secrets GVR, distinguished
// by gvr.Resource - mirrors how a real invocation reaches the same "local"
// cluster client for both create steps.
type fakeUserClient struct {
	userPreviewName string
	userRealName    string
	userCreateErr   error
	secretCreateErr error

	userCreateCalls   int
	secretCreateCalls int
	lastSecretObj     *unstructured.Unstructured
}

func (f *fakeUserClient) Create(_ context.Context, gvr schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	switch gvr.Resource {
	case "users":
		f.userCreateCalls++
		if dryRun {
			out := obj.DeepCopy()
			out.SetName(f.userPreviewName)
			return out, nil
		}
		if f.userCreateErr != nil {
			return nil, f.userCreateErr
		}
		out := obj.DeepCopy()
		name := f.userRealName
		if name == "" {
			name = f.userPreviewName
		}
		out.SetName(name)
		return out, nil
	case "secrets":
		f.secretCreateCalls++
		if dryRun {
			return obj.DeepCopy(), nil
		}
		f.lastSecretObj = obj.DeepCopy()
		if f.secretCreateErr != nil {
			return nil, f.secretCreateErr
		}
		out := obj.DeepCopy()
		delete(out.Object, "stringData")
		out.Object["data"] = map[string]any{"password": "aGFzaGVk", "salt": "c2FsdA=="}
		out.Object["annotations"] = map[string]any{"cattle.io/password-hash": "pbkdf2sha3512"}
		return out, nil
	default:
		return nil, fmt.Errorf("fakeUserClient: unexpected gvr %v", gvr)
	}
}

func (f *fakeUserClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	return nil, fmt.Errorf("fakeUserClient: Get not implemented")
}
func (f *fakeUserClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	return nil, nil
}
func (f *fakeUserClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (f *fakeUserClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}
func (f *fakeUserClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}

type stubUserFactory struct{ c engine.ClusterClient }

func (f stubUserFactory) Cluster(context.Context, string) (engine.ClusterClient, error) {
	return f.c, nil
}
func (f stubUserFactory) ResolveCluster(_ context.Context, s string) (string, error) { return s, nil }

type fakeSrc struct {
	body []byte
	err  error
}

func (f fakeSrc) Read(string) ([]byte, error) { return f.body, f.err }

type userInput struct {
	args  []string
	flags map[string]string
	bools map[string]bool
}

func (i userInput) String(k string) string  { return i.flags[k] }
func (i userInput) IsSet(k string) bool     { _, ok := i.flags[k]; return ok }
func (i userInput) Bool(k string) bool      { return i.bools[k] }
func (i userInput) Strings(string) []string { return nil }
func (i userInput) Args() []string          { return i.args }

func planInput(username string) userInput {
	return userInput{
		args:  []string{username},
		flags: map[string]string{},
		bools: map[string]bool{"password-stdin": true},
	}
}

func plan(t *testing.T, client engine.ClusterClient, src engine.FileSource, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	cmd := NewCreate(stubUserFactory{c: client}, src).(runtime.MutatingCommand)
	return cmd.Plan(t.Context(), in)
}

// TestCreatePlanDisplayIdentity asserts the dry-run identity conformance
// requirement for this bespoke Effect-building command: DisplayKind/
// DisplayName are non-empty at Plan time, before anything commits.
func TestCreatePlanDisplayIdentity(t *testing.T) {
	t.Parallel()
	client := &fakeUserClient{userPreviewName: "u-preview1"}
	eff, exec, err := plan(t, client, fakeSrc{body: []byte("correctHorseBattery1\n")}, planInput("alice"))
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, "user", eff.DisplayKind)
	assert.NotEmpty(t, eff.DisplayName)
}

// TestCreatePlanWarningNeverNamesTheDryRunPreviewSecret proves the Plan-time
// warning never states a specific Secret name: previewUser.GetName() is a
// placeholder that will not match the real committed name, and an operator
// seeing two different names for "the same" secret across the dry-run and
// apply outputs would read it as a bug.
func TestCreatePlanWarningNeverNamesTheDryRunPreviewSecret(t *testing.T) {
	t.Parallel()
	client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "u-real2"}
	eff, _, err := plan(t, client, fakeSrc{body: []byte("correctHorseBattery1\n")}, planInput("alice"))
	require.NoError(t, err)
	require.Len(t, eff.Warnings, 1)
	assert.NotContains(t, eff.Warnings[0], "u-preview1")
	assert.Contains(t, eff.Warnings[0], secretNamespace)
}

// TestCreatePasswordStdinRequired proves the flag is mandatory: there is no
// --password flag, ever, so omitting --password-stdin must refuse before
// any network call.
func TestCreatePasswordStdinRequired(t *testing.T) {
	t.Parallel()
	client := &fakeUserClient{userPreviewName: "u-preview1"}
	in := planInput("alice")
	in.bools["password-stdin"] = false
	_, _, err := plan(t, client, fakeSrc{body: []byte("whatever\n")}, in)
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, 0, client.userCreateCalls, "no network call before the password-stdin gate")
}

// TestCreateExecuteRedactsSecretPayloadAndReturnsRealCommittedIdentity
// proves the composite's result path passes through
// engine.RedactMutationResult (the hashed password/salt never reach -o
// json/yaml) and that the committed identity is the REAL server-assigned
// name - never the dry-run preview's placeholder.
func TestCreateExecuteRedactsSecretPayloadAndReturnsRealCommittedIdentity(t *testing.T) {
	t.Parallel()
	client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "u-real2"}
	_, exec, err := plan(t, client, fakeSrc{body: []byte("correctHorseBattery1\n")}, planInput("alice"))
	require.NoError(t, err)

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "UserCreated", res.Kind)
	assert.Contains(t, res.Text, "user/alice created")

	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, data, "data", "the hashed password must never reach -o json/yaml")
	assert.NotContains(t, data, "stringData")
	userInfo, ok := data["user"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "u-real2", userInfo["name"], "the committed identity must be the real server-assigned name")
	assert.NotContains(t, fmt.Sprint(userInfo["name"]), "*", "the committed identity must never carry the dry-run preview's marker")
	assert.Equal(t, "u-real2", client.lastSecretObj.GetName(), "the Secret must be created under the real committed User name, not the preview placeholder")
}

// TestCreateSecretFailureAfterUserCommittedNamesRecovery proves the
// committed-context requirement: a Secret failure after the User committed
// must never invite a blind retry (which would create a second User). The
// recovery creates the missing Secret directly via `create resource -f -` -
// NOT `create password-change-request`, which changes an existing
// password Secret and fails ("failed to get password secret") against a
// user that has none - and must never carry the password as a literal argv
// token the way `create secret --from-literal` would.
func TestCreateSecretFailureAfterUserCommittedNamesRecovery(t *testing.T) {
	t.Parallel()
	client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "u-real2",
		secretCreateErr: clierr.New(clierr.CodeGeneric, "boom")}
	_, exec, err := plan(t, client, fakeSrc{body: []byte("correctHorseBattery1\n")}, planInput("alice"))
	require.NoError(t, err)

	_, err = exec(t.Context())
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed)
	assert.Equal(t, "User", details.ResourceKind)
	assert.Equal(t, "u-real2", details.ResourceName)
	assert.Contains(t, details.Recovery, "create resource -f -",
		"recovery must create the missing Secret directly, not point at password-change-request")
	assert.NotContains(t, details.Recovery, "password-change-request")
	assert.NotContains(t, details.Recovery, "correctHorseBattery1",
		"the recovery must never embed the actual password value")
}

// TestCreateOldServerSecretDryRunNotFoundRefusesFriendly proves the
// enforcing-net backstop: a NOT_FOUND on the Secret dry-run (the
// cattle-local-user-passwords namespace does not exist on this server)
// reclassifies into the same plain refusal the declared-signal capability
// gate would give, never a raw namespace-not-found message.
func TestCreateOldServerSecretDryRunNotFoundRefusesFriendly(t *testing.T) {
	t.Parallel()
	client := &fakeUserClient{userPreviewName: "u-preview1"}
	client.secretCreateErr = clierr.New(clierr.CodeNotFound, "namespaces \"cattle-local-user-passwords\" not found")
	// secretCreateErr also applies to the dry-run branch in this fake's
	// Create - override it to return the error unconditionally.
	dryRunClient := &notFoundOnSecretDryRunClient{fakeUserClient: client}
	_, _, err := plan(t, dryRunClient, fakeSrc{body: []byte("correctHorseBattery1\n")}, planInput("alice"))
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
	assert.Contains(t, ce.Message, "does not support the user-password workflow")
}

// notFoundOnSecretDryRunClient wraps fakeUserClient to fail the Secret
// dry-run specifically (fakeUserClient's own dry-run branch always
// succeeds) - the one path this suite needs to fail that no flag on the
// base fake expresses.
type notFoundOnSecretDryRunClient struct{ *fakeUserClient }

func (c *notFoundOnSecretDryRunClient) Create(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	if gvr.Resource == "secrets" && dryRun {
		return nil, clierr.New(clierr.CodeNotFound, "namespaces \"cattle-local-user-passwords\" not found")
	}
	return c.fakeUserClient.Create(ctx, gvr, ns, obj, dryRun)
}

// TestCreateExplicitEmptyDisplayNameIsKept proves an explicitly empty
// --display-name stays empty: only an OMITTED flag defaults to the
// username.
func TestCreateExplicitEmptyDisplayNameIsKept(t *testing.T) {
	t.Parallel()
	client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "u-real2"}
	in := planInput("alice")
	in.flags["display-name"] = ""
	_, exec, err := plan(t, client, fakeSrc{body: []byte("correctHorseBattery1\n")}, in)
	require.NoError(t, err)

	res, err := exec(t.Context())
	require.NoError(t, err)
	userInfo, ok := res.Data.(map[string]any)["user"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "", userInfo["displayName"], "an explicitly empty display name must not be replaced with the username")
}

func (*fakeUserClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (*fakeUserClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted in this fake")
}

func (f stubUserFactory) ImpersonatedCluster(ctx context.Context, id string, _ engine.Impersonation) (engine.ClusterClient, error) {
	return f.Cluster(ctx, id)
}

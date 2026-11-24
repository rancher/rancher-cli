package testrancher

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
)

// The Norman cloud credential routes. Create stores the credential as a
// Secret in cattle-global-data through the fallthrough, as Rancher's Norman
// store does: the name cc-<5 letters>, the label cattle.io/creator=norman,
// the annotations that carry the display name, the description, the driver
// and the creator, and a stringData key <driver>credentialConfig-<field> for
// each config field. Update merges the given config fields, the name and
// description annotations and the given annotations into the stored Secret,
// and delete deletes it, both through the fallthrough; with none configured
// they respond without storing.
const (
	normanCloudCredentialsPath     = "/v3/cloudcredentials"
	normanCloudCredentialItemPath  = "/v3/cloudCredentials/"
	cloudCredentialSecretsPath     = "/k8s/clusters/local/api/v1/namespaces/cattle-global-data/secrets"
	cloudCredentialIDPrefix        = "cattle-global-data:"
	cloudCredentialConfigKeySuffix = "credentialConfig"
)

// cloudCredentialSequence numbers the credentials every facade of the process
// creates, so concurrent facades that store their Secrets in one cluster never
// generate the same name.
var cloudCredentialSequence atomic.Int64

// NormanCloudCredentialRecord is one Norman cloud credential request the
// facade responded to: its method, its path, the bearer that sent it and its
// decoded body (nil when the request had none or it was not JSON).
type NormanCloudCredentialRecord struct {
	Method string
	Path   string
	Bearer string
	Body   map[string]any
}

// NormanCloudCredentialRequests returns every Norman cloud credential request
// the facade responded to, in order, those it refused included.
func (f *Facade) NormanCloudCredentialRequests() []NormanCloudCredentialRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.cloudCredentialRequests)
}

func isNormanCloudCredentialRoute(path string) bool {
	return path == normanCloudCredentialsPath || strings.HasPrefix(path, normanCloudCredentialItemPath)
}

// handleNormanCloudCredentials serves create (POST on the collection), update
// (PUT) and delete (DELETE) of a cloud credential.
func (f *Facade) handleNormanCloudCredentials(w http.ResponseWriter, r *http.Request) {
	if !f.authorizedBearer(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.cloudCredentialRequests = append(f.cloudCredentialRequests, NormanCloudCredentialRecord{
		Method: r.Method, Path: r.URL.Path, Bearer: bearerFromHeader(r), Body: body})
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && r.URL.Path == normanCloudCredentialsPath:
		f.createCloudCredential(w, r, body)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, normanCloudCredentialItemPath):
		f.updateCloudCredential(w, r, strings.TrimPrefix(r.URL.Path, normanCloudCredentialItemPath), body)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, normanCloudCredentialItemPath):
		id := strings.TrimPrefix(r.URL.Path, normanCloudCredentialItemPath)
		if slices.Contains(f.opts.NormanCloudCredentialsInUse, id) {
			writeJSON(w, http.StatusUnprocessableEntity, normanError{http.StatusUnprocessableEntity, "InvalidAction",
				"Cloud credential is currently referenced by provisioning cluster fixture"}.body())
			return
		}
		if err := f.deleteCloudCredentialSecret(r, id); err != nil {
			refusal := normanStoreChangeRefusal(err)
			writeJSON(w, refusal.status, refusal.body())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// normanError is a Norman error response.
type normanError struct {
	status  int
	code    string
	message string
}

func (e normanError) body() map[string]any {
	return map[string]any{"type": "error", "status": e.status, "code": e.code, "message": e.message, "baseType": "error"}
}

// configOf returns the name and fields of the one <driver>credentialConfig
// object of body.
func configOf(body map[string]any) (driver string, fields map[string]any, ok bool) {
	for key, value := range body {
		if prefix, found := strings.CutSuffix(key, cloudCredentialConfigKeySuffix); found {
			if object, isObject := value.(map[string]any); isObject {
				return prefix, object, true
			}
		}
	}
	return "", nil, false
}

// normanConfigRefusal is the one validator of create and update: Norman's
// refusal of a body with no config object, MissingRequired, 422. An object
// that is present and empty passes, as a description-only update sends it.
func normanConfigRefusal(body map[string]any) *normanError {
	if _, _, ok := configOf(body); ok {
		return nil
	}
	return &normanError{http.StatusUnprocessableEntity, "MissingRequired", "a Config field must be set"}
}

// normanStoreRefusal is the response when the store the facade falls through to
// refuses a change.
func normanStoreRefusal() normanError {
	return normanError{http.StatusInternalServerError, "ServerError", "the test facade could not store the change"}
}

// storeStatusError is the status of a store that did not respond with 2xx.
type storeStatusError struct{ status int }

func (e *storeStatusError) Error() string {
	return fmt.Sprintf("the store responded with %d", e.status)
}

// normanStoreChangeRefusal is the response to an update or a delete the store
// refused: Norman's 404 NotFound when the Secret is gone, the store refusal
// for any other failure.
func normanStoreChangeRefusal(err error) normanError {
	if status, ok := errors.AsType[*storeStatusError](err); ok && status.status == http.StatusNotFound {
		return normanError{http.StatusNotFound, "NotFound", "the cloud credential was not found"}
	}
	return normanStoreRefusal()
}

// harvesterRefusal is Rancher's check of a Harvester kubeconfig: a 500 whose
// message starts with the prefix the store wraps every such error in, with the
// parser's tail quoting a fragment of the kubeconfig, or the message of a
// token that is expired, missing, or already used by another credential.
func (f *Facade) harvesterRefusal(fields map[string]any, update bool) *normanError {
	content, _ := fields["kubeconfigContent"].(string)
	const prefix = "failed to process harvester cloud credential: "
	switch {
	case update && strings.Contains(content, "kubeconfig-user-same"):
		return &normanError{http.StatusInternalServerError, "ServerError", prefix + "token kubeconfig-user-same is already in use by secret"}
	case !strings.Contains(content, "users:"):
		fragment := strings.TrimSpace(content)
		if len(fragment) > 24 {
			fragment = fragment[:24]
		}
		return &normanError{http.StatusInternalServerError, "ServerError",
			prefix + "failed to read the tokens from kubeconfig: failed to unmarshal kubeconfig: " + fragment}
	case strings.Contains(content, "kubeconfig-user-expired"):
		return &normanError{http.StatusInternalServerError, "ServerError", prefix + "token kubeconfig-user-expired is expired"}
	case strings.Contains(content, "kubeconfig-user-missing"):
		return &normanError{http.StatusInternalServerError, "ServerError", prefix + "token kubeconfig-user-missing was not found"}
	case strings.Contains(content, "kubeconfig-user-used"):
		return &normanError{http.StatusInternalServerError, "ServerError", prefix + "token kubeconfig-user-used is already in use by secret cc-other"}
	}
	return nil
}

// normanResponse is the response of a create or an update: Norman's transformer
// decodes every config field, so the response carries the config values.
func normanResponse(id string, body map[string]any) map[string]any {
	answer := map[string]any{"id": id, "type": "cloudCredential", "baseType": "cloudCredential"}
	for key, value := range body {
		if key != "type" {
			answer[key] = value
		}
	}
	return answer
}

func (f *Facade) createCloudCredential(w http.ResponseWriter, r *http.Request, body map[string]any) {
	if refusal := normanConfigRefusal(body); refusal != nil {
		writeJSON(w, refusal.status, refusal.body())
		return
	}
	driver, fields, _ := configOf(body)
	if driver == "harvester" {
		if refusal := f.harvesterRefusal(fields, false); refusal != nil {
			writeJSON(w, refusal.status, refusal.body())
			return
		}
	}
	n := cloudCredentialSequence.Add(1)
	name := fmt.Sprintf("cc-%c%c%c%c%c", 'a'+rune(n/26/26/26/26%26), 'a'+rune(n/26/26/26%26), 'a'+rune(n/26/26%26), 'a'+rune(n/26%26), 'a'+rune(n%26))

	annotations := map[string]any{"field.cattle.io/creatorId": "u-facade"}
	if display, _ := body["name"].(string); display != "" {
		annotations["field.cattle.io/name"] = display
	}
	if description, _ := body["description"].(string); description != "" {
		annotations["field.cattle.io/description"] = description
	}
	if given, ok := body["annotations"].(map[string]any); ok {
		maps.Copy(annotations, given)
	}
	stringData := map[string]any{}
	for field, value := range fields {
		stringData[driver+cloudCredentialConfigKeySuffix+"-"+field] = fmt.Sprint(value)
	}
	secret, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"metadata": map[string]any{"name": name, "namespace": "cattle-global-data",
			"labels": map[string]any{"cattle.io/creator": "norman"}, "annotations": annotations},
		"stringData": stringData,
	})
	if err == nil && f.opts.Fallthrough != nil {
		err = f.storeCloudCredentialSecret(r, secret)
	}
	if err != nil {
		refusal := &normanError{http.StatusInternalServerError, "ServerError", "the Secret of the cloud credential was not stored"}
		writeJSON(w, refusal.status, refusal.body())
		return
	}
	writeJSON(w, http.StatusCreated, normanResponse(cloudCredentialIDPrefix+name, body))
}

// storeCloudCredentialSecret posts the Secret of a new cloud credential to the
// store the facade falls through to and returns an error when the store does
// not respond with 2xx.
func (f *Facade) storeCloudCredentialSecret(r *http.Request, secret []byte) error {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, WithFieldManager(http.MethodPost, cloudCredentialSecretsPath), bytes.NewReader(secret))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	f.fallthroughTo(rec, req)
	if rec.Code < http.StatusOK || rec.Code > 299 {
		return fmt.Errorf("the store responded with %d", rec.Code)
	}
	return nil
}

// updateCloudCredential serves a PUT: the one validator, the Harvester check,
// then the change merged into the stored Secret. A PUT of the same Harvester
// kubeconfig is refused as Rancher refuses a token another secret, or this
// one, already has.
func (f *Facade) updateCloudCredential(w http.ResponseWriter, r *http.Request, id string, body map[string]any) {
	if refusal := normanConfigRefusal(body); refusal != nil {
		writeJSON(w, refusal.status, refusal.body())
		return
	}
	driver, fields, _ := configOf(body)
	if driver == "harvester" {
		if refusal := f.harvesterRefusal(fields, true); refusal != nil {
			writeJSON(w, refusal.status, refusal.body())
			return
		}
	}
	annotations := map[string]any{}
	if display, ok := body["name"].(string); ok {
		annotations["field.cattle.io/name"] = display
	}
	if description, ok := body["description"].(string); ok {
		annotations["field.cattle.io/description"] = description
	}
	if given, ok := body["annotations"].(map[string]any); ok {
		maps.Copy(annotations, given)
	}
	stringData := map[string]any{}
	for field, value := range fields {
		stringData[driver+cloudCredentialConfigKeySuffix+"-"+field] = fmt.Sprint(value)
	}
	change, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}, "stringData": stringData})
	if err == nil && f.opts.Fallthrough != nil {
		path := cloudCredentialSecretsPath + "/" + strings.TrimPrefix(id, cloudCredentialIDPrefix)
		err = f.sendSecretChange(r, http.MethodPatch, WithFieldManager(http.MethodPatch, path), "application/merge-patch+json", change)
	}
	if err != nil {
		refusal := normanStoreChangeRefusal(err)
		writeJSON(w, refusal.status, refusal.body())
		return
	}
	writeJSON(w, http.StatusOK, normanResponse(id, body))
}

// deleteCloudCredentialSecret deletes the Secret of a cloud credential from
// the store the facade falls through to; with none configured it deletes
// nothing.
func (f *Facade) deleteCloudCredentialSecret(r *http.Request, id string) error {
	if f.opts.Fallthrough == nil {
		return nil
	}
	return f.sendSecretChange(r, http.MethodDelete, cloudCredentialSecretsPath+"/"+strings.TrimPrefix(id, cloudCredentialIDPrefix), "", nil)
}

// sendSecretChange sends one request to the store the facade falls through to,
// with the caller's credentials, and returns a *storeStatusError when the
// store does not respond with 2xx.
func (f *Facade) sendSecretChange(r *http.Request, method, target, contentType string, body []byte) error {
	req, err := http.NewRequestWithContext(r.Context(), method, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	f.fallthroughTo(rec, req)
	if rec.Code < http.StatusOK || rec.Code > 299 {
		return &storeStatusError{rec.Code}
	}
	return nil
}

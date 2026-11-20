package testrancher

import (
	"bytes"
	"encoding/json"
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
// each config field. Update and delete answer without storing.
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
// facade answered: its method, its path, the bearer that sent it and its
// decoded body (nil when the request had none or it was not JSON).
type NormanCloudCredentialRecord struct {
	Method string
	Path   string
	Bearer string
	Body   map[string]any
}

// NormanCloudCredentialRequests returns every Norman cloud credential request
// the facade answered, in order, those it refused included.
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
		if refusal := normanConfigRefusal(body); refusal != nil {
			writeJSON(w, refusal.status, refusal.body())
			return
		}
		writeJSON(w, http.StatusOK, normanAnswer(strings.TrimPrefix(r.URL.Path, normanCloudCredentialItemPath), body))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, normanCloudCredentialItemPath):
		id := strings.TrimPrefix(r.URL.Path, normanCloudCredentialItemPath)
		if slices.Contains(f.opts.NormanCloudCredentialsInUse, id) {
			writeJSON(w, http.StatusUnprocessableEntity, normanError{http.StatusUnprocessableEntity, "InvalidAction",
				"Cloud credential is currently referenced by provisioning cluster fixture"}.body())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// normanError is a Norman error answer.
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

// normanConfigRefusal is Norman's refusal of a body with no config field:
// MissingRequired, 422.
func normanConfigRefusal(body map[string]any) *normanError {
	if _, fields, ok := configOf(body); ok && len(fields) > 0 {
		return nil
	}
	return &normanError{http.StatusUnprocessableEntity, "MissingRequired", "a Config field must be set"}
}

// harvesterRefusal is Rancher's check of a Harvester kubeconfig: a 500 whose
// message starts with the prefix the store wraps every such error in, with the
// parser's tail quoting a fragment of the kubeconfig, or the message of a
// token that is expired, missing, or already used by another credential.
func (f *Facade) harvesterRefusal(fields map[string]any) *normanError {
	content, _ := fields["kubeconfigContent"].(string)
	const prefix = "failed to process harvester cloud credential: "
	switch {
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

// normanAnswer is the answer of a create or an update: Norman's transformer
// decodes every config field, so the answer carries the config values.
func normanAnswer(id string, body map[string]any) map[string]any {
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
		if refusal := f.harvesterRefusal(fields); refusal != nil {
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
	if err == nil {
		req, rerr := http.NewRequestWithContext(r.Context(), http.MethodPost, WithFieldManager(http.MethodPost, cloudCredentialSecretsPath), bytes.NewReader(secret))
		if rerr == nil {
			req.Header.Set("Content-Type", "application/json")
			if auth := r.Header.Get("Authorization"); auth != "" {
				req.Header.Set("Authorization", auth)
			}
			f.fallthroughTo(httptest.NewRecorder(), req)
		}
	}
	writeJSON(w, http.StatusCreated, normanAnswer(cloudCredentialIDPrefix+name, body))
}

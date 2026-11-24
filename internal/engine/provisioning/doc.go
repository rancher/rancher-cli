// Package provisioning has the cloud credential resolver, its `get
// cloud-credential` listing and `create cloud-credential` and `patch
// cloud-credential`, beside the commands that provision and operate on
// clusters.
// The reads keep one rule: every function that reads a Kubernetes Secret
// object in cattle-global-data keeps only key names and metadata (name, the
// field.cattle.io/name annotation, creationTimestamp), never a `data` value; a
// `data` value must never flow into CloudCredRef, a runtime.Result, or an error
// string. cloudcredwrite.go keeps the secret values a write sends only in
// engine.CloudCredentialBody, in the list the error filter builds and, while
// Plan reads them, in credentialValue. readStoredCredential, which patch uses,
// also returns the Secret's resourceVersion, uid and annotations; the
// annotations go only into the update body and, for the name and the
// description, the preview's diff.
package provisioning

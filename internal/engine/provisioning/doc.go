// Package provisioning holds the cloud credential resolver, its `get
// cloud-credential` listing and `create cloud-credential`, beside the commands
// that provision and operate on clusters.
// SECRET HYGIENE: the reads keep one rule: every function that reads a
// Kubernetes Secret object in cattle-global-data retains ONLY key names and
// metadata (name, the field.cattle.io/name annotation, creationTimestamp),
// never a `data` VALUE; a `data` value must never flow into CloudCredRef, a
// runtime.Result, or an error string. cloudcredwrite.go holds the secret
// values a write sends in the hidden body type (engine.CloudCredentialBody), in
// the list the error filter builds and, while Plan reads them, in
// credentialValue.
package provisioning

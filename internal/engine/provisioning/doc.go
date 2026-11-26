// Package provisioning has the cloud credential resolver, its `get
// cloud-credential` listing and `create cloud-credential` and `patch
// cloud-credential`, beside the commands that provision and operate on
// clusters.
// The reads keep one rule: every function that reads a Kubernetes Secret
// object in cattle-global-data keeps only key names and metadata (name, the
// field.cattle.io/name annotation, creationTimestamp), never a `data` value; a
// `data` value must never flow into CloudCredRef, a runtime.Result, or an error
// string. cloudcredwrite.go keeps every value a write reads from a flag, a
// file or stdin as a secret.Value, which prints <hidden> in every format, and
// reveals the text only in the functions that call secret.Reveal.
// readStoredCredential, which patch uses, also returns the Secret's
// resourceVersion, uid and annotations; the annotations go only into the
// update body and, for the name and the description, the preview's diff.
package provisioning

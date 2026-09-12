// Package complete declares the live shell-completion source of the logs
// command - --pod, the pods of the target typed as the first positional,
// on the local cluster or the typed downstream cluster as the target's
// row says - and carries the curated target table itself, which the
// observe package imports back for the command and the support bundle.
// It is a restricted package: it may import the completion seam, the kind
// table, and a fixed set of standard-library packages, and nothing that
// reaches a client, a credential store, the environment, the file system
// or a process. A conformance test enforces the list, and the surface
// verifies at build time that every source it runs is declared here or
// in a sibling complete package.
package complete

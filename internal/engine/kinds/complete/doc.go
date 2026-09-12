// Package complete declares the generic live shell-completion source of
// the kind table: for any Completable kind, the names of its objects
// derived from the row's group, version, resource, namespace policy and
// scope alone, and the shared --namespace and --node sources, which are
// that derivation for the namespace and node kinds. It is a restricted
// package: it may import the completion seam, the kind table, and a fixed
// set of standard-library packages, and nothing that reaches a client, a
// credential store, the environment, the file system or a process. A
// conformance test enforces the list, and the surface verifies at build
// time that every source it runs is declared here or in a sibling
// complete package.
package complete

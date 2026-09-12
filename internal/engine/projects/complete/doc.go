// Package complete declares the live shell-completion source of projects:
// the shared --project value and the project kind's name positional, both
// the management Projects in the typed cluster's namespace. It is a
// restricted package: it may import the completion seam, the kind table,
// and a fixed set of standard-library packages, and nothing that reaches a
// client, a credential store, the environment, the file system or a
// process. A conformance test enforces the list, and the surface verifies
// at build time that every source it runs is declared here or in a
// sibling complete package.
package complete

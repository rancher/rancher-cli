// Package complete declares the live shell-completion sources of users:
// the user kind's name positional, the ids of the management Users that
// are not system accounts. It is a restricted package: it may import the
// completion seam, the kind table, and a fixed set of standard-library
// packages, and nothing that reaches a client, a credential store, the
// environment, the file system or a process. A conformance test enforces
// the list, and the surface verifies at build time that every source it
// runs is declared here or in a sibling complete package.
package complete

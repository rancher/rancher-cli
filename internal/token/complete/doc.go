// Package complete declares the live shell-completion source of --auth-provider
// on the sign-in commands: the names of the auth configs the server lists that
// this CLI can sign in with. It is a restricted package: it may import the
// completion seam, the kind table, and a fixed set of standard-library
// packages, and nothing that reaches a client, a credential store, the
// environment, the file system or a process. A conformance test enforces the
// list, and the surface verifies at build time that every source it runs is
// declared here or in a sibling complete package.
package complete

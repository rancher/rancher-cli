// Package complete declares the live shell-completion sources of the
// access commands and the shared --cluster value: the management cluster
// list every command offers for --cluster and the steve cluster
// collection `access projects` resolves its own --cluster against. It is
// a restricted package: it may import the completion seam, the kind
// table, and a fixed set of standard-library packages, and nothing that
// reaches a client, a credential store, the environment, the file system
// or a process. A conformance test enforces the list, and the surface
// verifies at build time that every source it runs is declared here or in
// a sibling complete package.
package complete

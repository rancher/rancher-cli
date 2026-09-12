// Package complete declares the live shell-completion sources of the
// provisioning commands: create cluster's --machine-config (the live
// half: the machine configs of the kind named by the typed `<kind>/`
// prefix, answered behind that prefix) and --cloud-credential (the cloud
// credential Secrets of cattle-global-data, every id plus each unique
// display name), and --pool on the cluster-family verbs that take a pool
// (the machine pools of the provisioning cluster typed as the first
// positional). It is a restricted package: it may import the completion
// seam, the kind table, and a fixed set of standard-library packages,
// and nothing that reaches a client, a credential store, the
// environment, the file system or a process. A conformance test enforces
// the list, and the surface verifies at build time that every source it
// runs is declared here or in a sibling complete package.
package complete

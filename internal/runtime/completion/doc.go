// Package completion is the seam between the CLI surface and the engine
// packages for live (network) shell completion. The surface owns the
// policy: it parses the site, gates on the network switch and the
// deadline, resolves the connection, grants the request allowance and
// sanitizes the answer. An owner package declares a Source: Claim says
// whether the source owns a site and what it needs, without I/O; Complete
// answers bare names through the Connection the surface built. The package
// is a leaf of the standard library: it imports nothing of the module, so
// every owner can depend on it.
package completion

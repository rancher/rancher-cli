package config

// MinRancherVersion is the CLI's declared minimum supported Rancher server
// version (PS-8, warn-only floor). It moves forward deliberately, in a
// commit, when the CLI starts depending on newer server API — no
// per-feature version matrix.
const MinRancherVersion = "2.14"

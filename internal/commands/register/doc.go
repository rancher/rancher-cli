// Package register is the server registration path shared by auth login
// and config add-server / update-server: canonicalizing the server URL,
// the TLS preflight and CA selection, and creating or updating the
// server entry under the config lock.
package register

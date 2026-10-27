package engine

import "github.com/rancher/rancher-cli/internal/clierr"

// WaitShouldStop reports whether a poll wait ends at once on err, the error
// of one GET in its loop: true for an authentication failure (AUTH) and a
// certificate failure (TLS_TRUST), false for every other error and for nil.
// Every poll loop that calls ClusterClient.Get repeatedly calls it on each
// GET error, so one rule decides which errors a wait retries. A NOT_FOUND
// error has its own handling in each loop, and a 403 (RBAC) is retried: right
// after a create or a role grant it may pass within seconds, as may a 5xx, a
// 429 or a network error.
//
// It assumes a 401 does not pass on a retry: an expired or rejected
// credential stays rejected. A 401 that the Rancher proxy relays from a
// downstream cluster has not been measured and is treated the same way.
func WaitShouldStop(err error) bool {
	ce := clierr.From(err)
	if ce == nil {
		return false
	}
	return ce.Code == clierr.CodeAuth || ce.Code == clierr.CodeTLSTrust
}

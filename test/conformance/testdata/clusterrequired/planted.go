// Package clusterrequired holds one file that writes a refusal of an absent
// --cluster by hand, which the scan must find, and one that only mentions
// the flag, which it must not.
package clusterrequired

import "errors"

func bareRefusal() error {
	return errors.New("--cluster is required")
}

func bareRefusalOfTwoFlags() error {
	return errors.New("--cluster or --all-clusters is required; pass one")
}

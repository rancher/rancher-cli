package envtestlog

import (
	"errors"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Stop stops env. On Windows, envtest cannot signal a process to stop and
// leaves etcd and the kube-apiserver running, so Stop also ends every etcd and
// kube-apiserver child of this process (one control plane per test process
// is assumed) and returns an error when one does not exit. On other
// platforms it returns the error from env.Stop unchanged.
func Stop(env *envtest.Environment) error {
	stopErr := env.Stop()
	stopped, killErr := killControlPlane()
	if stopped > 0 {
		// The directories envtest could not remove while the processes ran.
		_ = env.Stop()
	}
	return stopOutcome(stopErr, stopped, killErr, errSignalUnsupported)
}

// stopOutcome combines the error from envtest's stop, the number of
// processes stopped afterwards (terminated, or already exited) and the error
// from stopping them. unsupported is the error the platform returns for a
// signal it does not support (on Windows, EWINDOWS for the stop signal). It
// is dropped when every leaf of stopErr is that error and at least one
// process was stopped; any other error is returned.
func stopOutcome(stopErr error, stopped int, killErr, unsupported error) error {
	var errs []error
	if stopErr != nil && (stopped == 0 || !allLeavesAre(stopErr, unsupported)) {
		errs = append(errs, stopErr)
	}
	if killErr != nil {
		errs = append(errs, killErr)
	}
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	}
	return errors.Join(errs...)
}

// allLeavesAre reports whether err is not nil and every error at the bottom of
// its tree matches target with errors.Is. The tree is followed through
// Kubernetes aggregates (Errors), joined errors (Unwrap() []error) and single
// wraps. errors.Is alone is not enough: an aggregate reports a match when any
// one of its errors matches.
func allLeavesAre(err, target error) bool {
	switch e := err.(type) {
	case nil:
		return false
	case interface{ Errors() []error }:
		return allOf(e.Errors(), target)
	case interface{ Unwrap() []error }:
		return allOf(e.Unwrap(), target)
	case interface{ Unwrap() error }:
		if inner := e.Unwrap(); inner != nil {
			return allLeavesAre(inner, target)
		}
	}
	return errors.Is(err, target)
}

func allOf(errs []error, target error) bool {
	if len(errs) == 0 {
		return false
	}
	for _, err := range errs {
		if !allLeavesAre(err, target) {
			return false
		}
	}
	return true
}

// isControlPlaneImage reports whether name, the file name of a running
// program, is etcd or the kube-apiserver, with or without an .exe suffix and
// in any letter case.
func isControlPlaneImage(name string) bool {
	base := name
	if strings.EqualFold(base[max(len(base)-4, 0):], ".exe") {
		base = base[:len(base)-4]
	}
	return strings.EqualFold(base, "etcd") || strings.EqualFold(base, "kube-apiserver")
}

//go:build !windows

package envtestlog

// errSignalUnsupported is the error envtest returns when the platform does
// not support the stop signal. Other platforms always do.
var errSignalUnsupported error

// killControlPlane does nothing: envtest stops its processes itself.
func killControlPlane() (int, error) { return 0, nil }

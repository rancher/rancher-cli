package envtestlog

import (
	"fmt"
	"os"
	"testing"
)

const (
	// fakeEtcdEnv, when set, makes this test binary behave as an etcd that
	// prints fakeEtcdMarker and exits at once, so a start failure can be
	// produced without a real etcd. envtest runs the binary named by the
	// process Path and passes it etcd's flags, so the check comes before any
	// flag parsing.
	fakeEtcdEnv    = "ENVTESTLOG_FAKE_ETCD"
	fakeEtcdMarker = "fake etcd: refusing to start"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeEtcdEnv) != "" {
		_, _ = fmt.Fprintln(os.Stdout, fakeEtcdMarker)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

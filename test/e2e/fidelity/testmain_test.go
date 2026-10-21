//go:build e2e_fidelity

package fidelity_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/testrancher/fidelity"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// driftConfig and driftCollector are set in drift mode (a drift run of
// scripts/e2e-fidelity) and nil otherwise; every use is behind a check of
// driftConfig.
var (
	driftConfig    *fidelity.DriftConfig
	driftCollector *fidelity.DriftCollector
)

// TestMain fails any request this package's tests send through
// http.DefaultTransport, and fails the package when one did: a test sends
// its requests through its httptest server's own client, because closing
// any httptest server closes the idle connections of the default transport.
// In drift mode it also refuses a configuration that records goldens, and
// after the run writes the drift report.
func TestMain(m *testing.M) {
	flag.Parse()
	cfg, drift, err := fidelity.DriftConfigFromEnv(os.Getenv, *update)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if drift {
		driftConfig = &cfg
		driftCollector = &fidelity.DriftCollector{}
	}
	restore := testtransport.GuardDefaultTransport()
	code := m.Run()
	guardErr := restore()
	if drift {
		os.Exit(finishDrift(cfg, code, guardErr))
	}
	if guardErr != nil {
		fmt.Fprintln(os.Stderr, guardErr)
		code = 1
	}
	os.Exit(code)
}

// finishDrift gathers what FinishDriftRun decides from, writes the report it
// returns and gives the exit code of the test binary.
func finishDrift(cfg fidelity.DriftConfig, runCode int, guardErr error) int {
	in := fidelity.DriftInputs{
		Config:    cfg,
		Known:     fidelity.LoadKnownFile(cfg.KnownPath),
		Collector: driftCollector,
		GuardErr:  guardErr,
		RunCode:   runCode,
	}
	if tgt, err := fidelity.NewOracleTargetFromEnv(); err != nil {
		in.TargetErr = err
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		in.Version, in.VersionErr = fidelity.FetchRancherVersion(ctx, tgt)
	}
	report, code := fidelity.FinishDriftRun(in)
	if err := fidelity.WriteReport(cfg.ReportPath, report); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "drift report written to %s\n", cfg.ReportPath)
	return code
}

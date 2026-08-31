// Command ghstub stands in for the gh CLI in the Windows installer tests. It
// appends its arguments to the file named by RANCHER_TEST_GH_ARGV_LOG and
// answers according to RANCHER_TEST_GH_MODE:
//
//   - token: "auth token" prints RANCHER_TEST_GH_TOKEN; everything else fails.
//   - capable: the attestation capability probe and "auth status" succeed,
//     "attestation verify <file>" succeeds, and "auth token" fails.
//   - api: "api repos/rancher/rancher-cli/contents/install.ps1?ref=main"
//     prints the file named by RANCHER_TEST_INSTALL_SCRIPT; everything else
//     fails.
//   - anything else: every invocation fails.
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	args := os.Args[1:]
	if log := os.Getenv("RANCHER_TEST_GH_ARGV_LOG"); log != "" {
		f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Fprintln(f, strings.Join(args, " "))
		_ = f.Close()
	}
	sub := strings.Join(args, " ")
	switch os.Getenv("RANCHER_TEST_GH_MODE") {
	case "token":
		if strings.HasPrefix(sub, "auth token") {
			fmt.Println(os.Getenv("RANCHER_TEST_GH_TOKEN"))
			return
		}
	case "capable":
		switch {
		case sub == "attestation verify --help":
			fmt.Println("--signer-workflow")
			return
		case sub == "auth status":
			return
		case strings.HasPrefix(sub, "attestation verify "):
			return
		}
	case "api":
		if strings.HasPrefix(sub, "api repos/rancher/rancher-cli/contents/install.ps1?ref=main") {
			raw, err := os.ReadFile(os.Getenv("RANCHER_TEST_INSTALL_SCRIPT"))
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			_, _ = os.Stdout.Write(raw)
			return
		}
	}
	os.Exit(1)
}

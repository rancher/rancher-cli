// Command ghstub stands in for the gh CLI in the Windows installer tests. It
// appends its arguments to the file named by RANCHER_TEST_GH_ARGV_LOG and
// answers according to RANCHER_TEST_GH_MODE. When RANCHER_TEST_GH_ARGS_LOG is
// set it also appends one line per call with every argument in brackets, so a
// test sees where one argument ends and the next begins. The modes are:
//
//   - token: "auth token" prints RANCHER_TEST_GH_TOKEN; everything else fails.
//   - capable: the attestation capability probe lists --cert-identity,
//     --signer-workflow and --source-ref, "auth status" succeeds, "auth token"
//     fails, and "attestation verify <file>" succeeds. When
//     RANCHER_TEST_GH_ATTESTED_REF is set, "attestation verify <file>" acts like
//     gh against an asset attested for that ref: it succeeds only when
//     --cert-identity is exactly the release.yaml identity for the ref and
//     --source-ref is exactly the ref, case included (the comparison of
//     --source-ref matches gh 2.102.0 and newer and is stricter than older gh,
//     which ignores case), and it fails when two of --cert-identity,
//     --cert-identity-regex, --signer-repo and --signer-workflow are given. The
//     checks read os.Args, so a test sees the arguments after command line
//     parsing, as the real gh, a Go program, does.
//     "attestation verify <file>" also fails unless --repo is exactly
//     rancher/rancher-cli and every other argument is a known flag with a value,
//     as gh refuses an unknown flag or a missing --repo.
//   - older: like capable, for a gh that predates --source-ref: the probe does
//     not list it and a verify call that passes it fails with "unknown flag".
//   - anything else: every invocation fails.
package main

import (
	"fmt"
	"os"
	"strings"
)

const identityPrefix = "https://github.com/rancher/rancher-cli/.github/workflows/release.yaml@"

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
	if log := os.Getenv("RANCHER_TEST_GH_ARGS_LOG"); log != "" {
		f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		for _, a := range args {
			fmt.Fprintf(f, "[%s]", a)
		}
		fmt.Fprintln(f)
		_ = f.Close()
	}
	sub := strings.Join(args, " ")
	mode := os.Getenv("RANCHER_TEST_GH_MODE")
	switch mode {
	case "token":
		if strings.HasPrefix(sub, "auth token") {
			fmt.Println(os.Getenv("RANCHER_TEST_GH_TOKEN"))
			return
		}
	case "capable", "older":
		switch {
		case sub == "attestation verify --help":
			fmt.Println("--cert-identity")
			fmt.Println("--signer-workflow")
			if mode == "capable" {
				fmt.Println("--source-ref")
			}
			return
		case sub == "auth status":
			return
		case strings.HasPrefix(sub, "attestation verify "):
			verify(args[2:], mode)
			return
		}
	}
	os.Exit(1)
}

// verify checks the arguments that follow "attestation verify".
func verify(args []string, mode string) {
	var identity, sourceRef, repo string
	groupFlags, files := 0, 0
	for i := 0; i < len(args); i++ {
		value := ""
		if i+1 < len(args) {
			value = args[i+1]
		}
		switch args[i] {
		case "--cert-identity":
			identity = value
			groupFlags++
			i++
		case "--cert-identity-regex", "--signer-repo", "--signer-workflow":
			groupFlags++
			i++
		case "--source-ref":
			if mode == "older" {
				fmt.Fprintln(os.Stderr, "unknown flag: --source-ref")
				os.Exit(1)
			}
			sourceRef = value
			i++
		case "--repo":
			repo = value
			i++
		default:
			if strings.HasPrefix(args[i], "-") {
				fmt.Fprintln(os.Stderr, "unknown flag: "+args[i])
				os.Exit(1)
			}
			files++
		}
	}
	if files != 1 || repo != "rancher/rancher-cli" {
		fmt.Fprintln(os.Stderr, "Error: want one file and --repo rancher/rancher-cli")
		os.Exit(1)
	}
	attested := os.Getenv("RANCHER_TEST_GH_ATTESTED_REF")
	if attested == "" {
		return
	}
	if groupFlags >= 2 {
		fmt.Fprintln(os.Stderr, "if any flags in the group [cert-identity cert-identity-regex signer-repo signer-workflow] are set none of the others can be")
		os.Exit(1)
	}
	if identity == identityPrefix+attested && sourceRef == attested {
		return
	}
	fmt.Fprintln(os.Stderr, `Error: verifying with issuer "sigstore.dev"`)
	os.Exit(1)
}

package cli

import (
	"context"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// Test exports — package-internal functions exposed for invariant and fetch tests.
// Only one export file exists (finding 13). Adjust identifiers to match what was
// actually implemented in earlier tasks; fix the alias, not the test, on any mismatch.

func SetStoredConfigForTest(cfg *config.Config, dir string) { setStoredConfig(cfg, dir) }
func LoadedConfigViewForTest() runtime.ConfigView {
	v, _ := loadedConfigView()
	return v
}

func ResolveCompletionTokenForTest(argTail []string, clusterID string) TokenCapability {
	return resolveCompletionToken(argTail, clusterID)
}
func TokenCapabilityForTest(url, token string, idOnly bool) TokenCapability {
	return TokenCapability{ServerURL: url, Token: token, IDOnly: idOnly}
}
func FetchClustersForTest(ctx context.Context, cap TokenCapability, partial string) []string {
	hc, err := newCompletionClient(cap.CACert)
	if err != nil {
		return nil
	}
	return fetchClusters(ctx, hc, cap, partial)
}
func SanitizeCandidatesForTest(s []string) []string { return sanitizeCandidates(s) }
func KindForDescriptorForTest(name string) (kinds.Kind, bool) {
	return kindForDescriptor(runtime.Descriptor{Name: name})
}
func SetRawArgvForTest(argv []string)             { SetRawArgv(argv) }
func SanitizedArgsForTest(args []string) []string { return SanitizedArgs(args) }
func CaptureArgvForTest(args []string)            { CaptureArgv(args) }
func GetRawProtoV2ForTest() bool                  { return rawProtoV2 }
func GlobalArgSpecsForTest() []runtime.ArgSpec    { return globalArgSpecs }

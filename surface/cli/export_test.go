package cli

import (
	"context"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// Test exports - package-internal functions exposed for invariant and fetch tests.
// Only one export file exists (finding 13). Adjust identifiers to match what was
// actually implemented in earlier tasks; fix the alias, not the test, on any mismatch.

func SetStoredConfigForTest(cfg *config.Config, dir string) { setStoredConfig(cfg, dir) }
func LoadedConfigViewForTest() runtime.ConfigView {
	v, _ := loadedConfigView()
	return v
}

func ResolveCompletionTokenForTest(clusterID string) TokenCapability {
	return resolveCompletionToken(clusterID)
}
func TokenCapabilityForTest(url, token string, idOnly bool) TokenCapability {
	return TokenCapability{ServerURL: url, Token: token, IDOnly: idOnly}
}
func FetchClustersForTest(ctx context.Context, cap TokenCapability) []string {
	hc, err := newCompletionClient(cap.CACert)
	if err != nil {
		return nil
	}
	return fetchClusters(ctx, hc, cap)
}
func SanitizeCandidatesForTest(s []string) []string { return sanitizeCandidates(s) }
func KindForDescriptorForTest(name string) (kinds.Kind, bool) {
	return kindForDescriptor(runtime.Descriptor{Name: name})
}
func SetRawArgvForTest(argv []string)             { SetRawArgv(argv) }
func SanitizedArgsForTest(args []string) []string { return SanitizedArgs(args) }
func CaptureArgvForTest(args []string)            { CaptureArgv(args) }
func GetRawProtoV2ForTest() bool                  { return rawProtoV2 }
func GetRawV1CompletionForTest() bool             { return rawV1Completion }
func SetRawV1CompletionForTest(v bool)            { rawV1Completion = v }
func GlobalArgSpecsForTest() []runtime.ArgSpec    { return globalArgSpecs }

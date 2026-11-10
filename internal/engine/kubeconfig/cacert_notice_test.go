package kubeconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/remedytest"
)

// TestKubeconfigNoticeOfAMissingLoginNamesTheVariablesCA asserts the notice
// of a kubeconfig that no stored login answers for, written from an
// environment-only session, names the session's CA: a variable that is a
// path as --cacert with that path, one that is a PEM as a file to make,
// said to be carried by RANCHER_CLI_CACERT.
func TestKubeconfigNoticeOfAMissingLoginNamesTheVariablesCA(t *testing.T) {
	keyring.MockInit()
	pemCA := string(testCAPEM(t))
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, []byte(pemCA), 0o600))

	noticeFor := func(t *testing.T, variable string) string {
		t.Helper()
		_, lines, _, err := runRecords(t, recordsSetup{envURL: recordsURL, envToken: true, envCA: variable})
		require.NoError(t, err)
		for _, l := range lines {
			if strings.Contains(l, "answers for this kubeconfig") {
				return l
			}
		}
		require.Fail(t, "no notice about the missing login", "%v", lines)
		return ""
	}

	assert.Contains(t, noticeFor(t, caPath), " --cacert '"+caPath+"'")
	inline := noticeFor(t, pemCA)
	assert.Contains(t, inline, "RANCHER_CLI_CACERT carries the CA inline and --cacert needs a file; ")
	assert.Contains(t, inline, " "+config.CAFileArg("--cacert")+" --token-stdin")
	remedytest.AssertClauseOnce(t, inline, config.CAFileMeaning, config.CAFileArg("--cacert"), true)
	assert.NotContains(t, inline, "this exec entry carries the CA inline")
	assert.NotContains(t, noticeFor(t, ""), "--cacert")
}

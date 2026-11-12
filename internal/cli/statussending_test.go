package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	releaseSendingLine     = "  sending: none - this build sends no event; recorded events stay on this machine\n"
	developmentSendingLine = "  sending: none - no upload endpoint is set; recorded events stay on this machine\n"
)

// TestStatusSendsEventsBuildKinds asserts, through Run, that status prints the
// release line and reports sendsEvents false for every release version, with no
// variable and with a valid endpoint; and that every development version
// prints the development line with no endpoint, and prints no sending line and
// reports sendsEvents true with an accepted endpoint and recording on.
func TestStatusSendsEventsBuildKinds(t *testing.T) {
	uploadEnv(t)
	for _, version := range releaseVersions {
		for name, value := range map[string]string{"no variable": "", "an endpoint": "telemetryupload=https://h.example"} {
			t.Run(version+" "+name, func(t *testing.T) {
				setExperiment(t, value, value != "")
				data, _, _ := statusOf(t, version)
				require.Contains(t, data, "sendsEvents")
				assert.Equal(t, false, data["sendsEvents"])
				text := runBuilt(t, version, "telemetry", "status")
				require.Equal(t, 0, text.code, text.stderr)
				assert.Contains(t, text.stdout, releaseSendingLine)
			})
		}
	}
	for _, version := range developmentVersions {
		t.Run(version+" no variable", func(t *testing.T) {
			setExperiment(t, "", false)
			data, _, _ := statusOf(t, version)
			assert.Equal(t, false, data["sendsEvents"])
			text := runBuilt(t, version, "telemetry", "status")
			require.Equal(t, 0, text.code, text.stderr)
			assert.Contains(t, text.stdout, developmentSendingLine)
		})
		t.Run(version+" an endpoint", func(t *testing.T) {
			withEndpoint(t, "https://h.example")
			data, _, _ := statusOf(t, version)
			assert.Equal(t, true, data["sendsEvents"])
			text := runBuilt(t, version, "telemetry", "status")
			require.Equal(t, 0, text.code, text.stderr)
			assert.NotContains(t, text.stdout, "sending:")
		})
	}
}

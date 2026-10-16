package tlstrust

import (
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retiredCertificatePhrases are wordings that never come back in a
// certificate failure message.
var retiredCertificatePhrases = []string{
	"out of band", "out-of-band", "trusted channel", "already trusted",
	"intercepting TLS", "TLS-intercepting", "server operator",
}

// TestClassifiedMessagesUsePlainWords asserts each kind's message says what
// to do in plain words: the administrator of the server, a source the user
// trusts, a proxy replacing the certificate; and that none of the retired
// phrases appears in any message, the leaf-only and the CA variants of an
// unknown authority included. Mutation: restore any retired phrase.
func TestClassifiedMessagesUsePlainWords(t *testing.T) {
	want := map[Kind]string{
		KindUnknownAuthority: "obtain its CA from a source you trust (or confirm the fingerprint shown here with the server's administrator)",
		KindExpired:          "ask the server's administrator to renew it",
		KindNotYetValid:      "ask the server's administrator to reissue it",
		KindInvalidChain:     "contact the server's administrator",
	}
	const tail = "; if this server worked before, check the subject and issuer shown here - " +
		"a certificate you don't recognize can mean something between you and the server, such as a proxy, is replacing its certificate"
	messages := map[string]string{
		"unknown authority, a CA in the chain":     UnknownAuthorityMessage(false, ""),
		"unknown authority, the certificate alone": UnknownAuthorityMessage(true, ""),
	}
	for _, tc := range failureKinds(t) {
		fx := startWrapFixture(t, tc.leaf)
		plain, err := httpx.New(httpx.Options{CACert: tc.caPEM, Timeout: 5 * time.Second})
		require.NoError(t, err)
		_, err = plain.Get(fx.url)
		require.Error(t, err)
		ce := Classify(err, "rancher.test", time.Now(), Remedy{})
		require.NotNil(t, ce)
		messages[tc.name] = ce.Message
		if sentence, ok := want[tc.kind]; ok {
			assert.Contains(t, ce.Message, sentence, tc.name)
		}
	}
	assert.Contains(t, messages["unknown authority, the certificate alone"],
		"obtain the certificate from a source you trust (or confirm the fingerprint shown here with the server's administrator)")
	assert.Contains(t, messages["unknown authority, a CA in the chain"], want[KindUnknownAuthority])
	assert.Contains(t, messages["unknown authority, a CA in the chain"], tail)
	assert.Contains(t, messages["unknown authority, the certificate alone"], tail)
	for name, msg := range messages {
		for _, phrase := range retiredCertificatePhrases {
			assert.NotContains(t, strings.ToLower(msg), strings.ToLower(phrase), name)
		}
	}
}

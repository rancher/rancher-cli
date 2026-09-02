package testrancher

import (
	"fmt"
	"net/http"
	"sync"
)

const defaultIdPPendingPolls = 2

// idpStub fakes an RFC 8628 device-authorization identity provider: one
// active device-code session at a time, which is all a single façade
// instance (one script) ever drives concurrently.
type idpStub struct {
	pendingTotal int

	mu          sync.Mutex
	counter     int
	pendingLeft int
	deviceCode  string
	userCode    string
	idToken     string
}

func newIdPStub(pendingPolls int) *idpStub {
	if pendingPolls <= 0 {
		pendingPolls = defaultIdPPendingPolls
	}
	return &idpStub{pendingTotal: pendingPolls}
}

func (s *idpStub) handleDeviceCode(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.counter++
	n := s.counter
	s.deviceCode = fmt.Sprintf("idp-devicecode-%d", n)
	s.userCode = fmt.Sprintf("IDP-%d", n)
	s.idToken = fmt.Sprintf("idp-idtoken-%d", n)
	s.pendingLeft = s.pendingTotal
	deviceCode, userCode := s.deviceCode, s.userCode
	s.mu.Unlock()

	base := "https://" + r.Host
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":      deviceCode,
		"user_code":        userCode,
		"verification_uri": base + "/idp/verify",
		"expires_in":       600,
		"interval":         1,
	})
}

func (s *idpStub) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if r.FormValue("grant_type") == "authorization_code" {
		code := r.FormValue("code")
		s.mu.Lock()
		defer s.mu.Unlock()
		if code == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		s.counter++
		s.idToken = fmt.Sprintf("idp-idtoken-%d", s.counter)
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": "idp-access-" + s.idToken,
			"token_type":   "bearer",
			"expires_in":   300,
			"id_token":     s.idToken,
		})
		return
	}

	deviceCode := r.FormValue("device_code")

	s.mu.Lock()
	defer s.mu.Unlock()
	if deviceCode == "" || deviceCode != s.deviceCode {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	if s.pendingLeft > 0 {
		s.pendingLeft--
		writeJSON(w, http.StatusOK, map[string]any{"error": "authorization_pending"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": "idp-access-" + s.idToken,
		"token_type":   "bearer",
		"expires_in":   300,
		"id_token":     s.idToken,
	})
}

func (s *idpStub) validIDToken(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return tok != "" && tok == s.idToken
}

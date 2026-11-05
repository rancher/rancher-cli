package testrancher

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync"
)

const defaultIdPPendingPolls = 2

// idpStub fakes an identity provider for the RFC 8628 device-authorization
// grant and the RFC 7636 authorization-code grant with PKCE: one active
// device-code session at a time, which is all a single facade instance (one
// script) ever drives concurrently. A code the authorize endpoint issued is
// exchanged only with the verifier of the challenge it was issued for; any
// other code is accepted as before.
type idpStub struct {
	pendingTotal int

	mu          sync.Mutex
	counter     int
	pendingLeft int
	deviceCode  string
	userCode    string
	idToken     string
	// challenges holds, by code, the code_challenge each code the authorize
	// endpoint issued was issued for, and spent those that were exchanged;
	// verifiers is every code_verifier an exchange of one of those codes
	// sent, in order.
	challenges map[string]string
	spent      map[string]bool
	verifiers  []string
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

// handleAuthorize answers the authorization request of the authorization-code
// grant with PKCE: a GET that carries response_type=code, a client_id, a
// redirect_uri, a state and an S256 code_challenge is redirected to the
// redirect_uri with a code and the same state. Any other request is a 400.
func (s *idpStub) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	switch {
	case q.Get("response_type") != "code":
		http.Error(w, "response_type must be code", http.StatusBadRequest)
	case q.Get("client_id") == "":
		http.Error(w, "client_id is required", http.StatusBadRequest)
	case err != nil || redirect.Host == "" || (redirect.Scheme != "http" && redirect.Scheme != "https"):
		http.Error(w, "redirect_uri must be an http or https URL", http.StatusBadRequest)
	case q.Get("state") == "":
		http.Error(w, "state is required", http.StatusBadRequest)
	case q.Get("code_challenge") == "":
		http.Error(w, "code_challenge is required", http.StatusBadRequest)
	case q.Get("code_challenge_method") != "S256":
		http.Error(w, "code_challenge_method must be S256", http.StatusBadRequest)
	default:
		s.mu.Lock()
		s.counter++
		code := fmt.Sprintf("idp-authcode-%d", s.counter)
		if s.challenges == nil {
			s.challenges = map[string]string{}
		}
		s.challenges[code] = q.Get("code_challenge")
		s.mu.Unlock()

		back := redirect.Query()
		back.Set("code", code)
		back.Set("state", q.Get("state"))
		redirect.RawQuery = back.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	}
}

// pkceChallenge is the S256 code_challenge of a code_verifier (RFC 7636).
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
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
		if challenge, issued := s.challenges[code]; issued {
			// A code is exchanged once, with the verifier of its challenge.
			if s.spent[code] {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "the code was already exchanged"})
				return
			}
			if s.spent == nil {
				s.spent = map[string]bool{}
			}
			s.spent[code] = true
			verifier := r.FormValue("code_verifier")
			s.verifiers = append(s.verifiers, verifier)
			if subtle.ConstantTimeCompare([]byte(pkceChallenge(verifier)), []byte(challenge)) != 1 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "code_verifier does not match the code_challenge"})
				return
			}
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

// codeVerifiers is the code_verifier of the first exchange of each code the
// authorize endpoint issued, in order, whether it matched or not.
func (s *idpStub) codeVerifiers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.verifiers)
}

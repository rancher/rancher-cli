package telemetry

import "sync"

var (
	serverMu  sync.Mutex
	serverURL string
	serverCA  string
)

// SetServer records the server the process's command targets: its URL as
// the caller canonicalized it, and the CA bundle it uses. An empty URL
// counts as not set. The last call wins. The holder reads no credential and
// makes no request.
func SetServer(url, ca string) {
	serverMu.Lock()
	defer serverMu.Unlock()
	serverURL, serverCA = url, ca
}

// Server returns what SetServer last stored; the URL is empty when none was.
func Server() (url, ca string) {
	serverMu.Lock()
	defer serverMu.Unlock()
	return serverURL, serverCA
}

// ResetServerForTest clears the holder. The calling test must not use
// t.Parallel(): the holder is process-wide.
func ResetServerForTest() { SetServer("", "") }

package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	clientauthv1 "k8s.io/client-go/pkg/apis/clientauthentication/v1"
	clientauthv1beta1 "k8s.io/client-go/pkg/apis/clientauthentication/v1beta1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/auth/exec"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
)

// execInfoEnv is the variable kubectl fills for an exec plugin; with
// provideClusterInfo set on the kubeconfig's exec block it carries the
// invoking context's cluster entry.
const execInfoEnv = "KUBERNETES_EXEC_INFO"

// selfSubjectReviewBody is the probe: the API server answers it only after
// authentication, so its answer proves whether the bearer is known there.
const selfSubjectReviewBody = `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`

// The direct-endpoint wait: a fresh cluster token is accepted by a
// cluster's Authorized Cluster Endpoint only after Rancher has synced it
// downstream, so the plugin probes the endpoint kubectl is about to use
// until it authenticates the token. Vars so tests shorten them; tests that
// set them must not run in parallel.
var (
	// directWaitStarted is a test-only synchronization point, invoked (when
	// non-nil) as the wait begins, before the caller's context is read.
	// Tests cancel the caller there to pin the cancelled-before-probing
	// interleaving. Nil in production.
	directWaitStarted func()

	directProbeInterval = 250 * time.Millisecond
	// directSettle is the interval after the first ready answer during which
	// no 401 may arrive before readiness is declared: time for the other
	// control-plane nodes' authenticator caches to warm, not a sample of
	// backends (a persistent connection pins one upstream).
	directSettle       = time.Second
	directWaitBound    = 10 * time.Second
	directRecheckBound = 3 * time.Second
)

// execCluster is the cluster entry kubectl passed in KUBERNETES_EXEC_INFO:
// the server to probe, the transport configuration to reach it, and the
// fingerprint readiness is remembered under.
type execCluster struct {
	server      string
	cfg         *rest.Config
	fingerprint string
}

// loadExecCluster reads the invoking context's cluster entry from
// KUBERNETES_EXEC_INFO through client-go's loader. A missing variable, or
// one without cluster information (a kubeconfig generated without
// provideClusterInfo), is nil, nil; a malformed value is an error the
// caller reports and then proceeds without a wait.
func loadExecCluster() (*execCluster, error) {
	raw := os.Getenv(execInfoEnv)
	if raw == "" {
		return nil, nil
	}
	var probe struct {
		Spec struct {
			Cluster json.RawMessage `json:"cluster"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", execInfoEnv, err)
	}
	if len(probe.Spec.Cluster) == 0 || string(probe.Spec.Cluster) == "null" {
		return nil, nil
	}
	obj, cfg, err := exec.LoadExecCredentialFromEnv()
	if err != nil {
		return nil, err
	}
	var (
		server, tlsName, proxyURL string
		ca                        []byte
		insecure                  bool
	)
	switch c := obj.(type) {
	case *clientauthv1beta1.ExecCredential:
		server, ca, tlsName, insecure, proxyURL = c.Spec.Cluster.Server, c.Spec.Cluster.CertificateAuthorityData, c.Spec.Cluster.TLSServerName, c.Spec.Cluster.InsecureSkipTLSVerify, c.Spec.Cluster.ProxyURL
	case *clientauthv1.ExecCredential:
		server, ca, tlsName, insecure, proxyURL = c.Spec.Cluster.Server, c.Spec.Cluster.CertificateAuthorityData, c.Spec.Cluster.TLSServerName, c.Spec.Cluster.InsecureSkipTLSVerify, c.Spec.Cluster.ProxyURL
	default:
		return nil, fmt.Errorf("%s carries an ExecCredential of an unsupported version %T", execInfoEnv, obj)
	}
	return &execCluster{server: server, cfg: cfg, fingerprint: endpointFingerprint(server, ca, tlsName, insecure, proxyURL)}, nil
}

// endpointFingerprint identifies an endpoint by every routing and trust
// field of the cluster entry, length-delimited so no two field sets
// collide. disable-compression is a transport option that cannot change
// which authenticator receives the token and is left out on purpose.
func endpointFingerprint(server string, ca []byte, tlsServerName string, insecure bool, proxyURL string) string {
	h := sha256.New()
	for _, f := range [][]byte{[]byte(server), ca, []byte(tlsServerName), []byte(strconv.FormatBool(insecure)), []byte(proxyURL)} {
		_, _ = fmt.Fprintf(h, "%d:", len(f))
		_, _ = h.Write(f)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// isRancherProxy reports whether server is exactly the Rancher proxy URL of
// clusterID under base: scheme and host compared case-insensitively, the
// path with its cluster id segment escaped and one trailing slash
// tolerated, and no userinfo, query or fragment at all. Never a prefix
// match: a longer path, another cluster id or any extra component is not
// the proxy.
func isRancherProxy(server, base, clusterID string) bool {
	expected, err := url.Parse(strings.TrimSuffix(base, "/") + "/k8s/clusters/" + url.PathEscape(clusterID))
	if err != nil {
		return false
	}
	got, err := url.Parse(server)
	if err != nil || got.User != nil || got.RawQuery != "" || got.ForceQuery || got.Fragment != "" {
		return false
	}
	canon := func(u *url.URL) string {
		return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimSuffix(u.EscapedPath(), "/")
	}
	return canon(expected) == canon(got)
}

type probeAnswer int

const (
	// probeRetry is any answer that proves nothing about authentication: a
	// transport error, an intermediary's 404, 429 or 5xx, a 403 without a
	// Kubernetes Status body, a 2xx without a username.
	probeRetry probeAnswer = iota
	// probeUnauthorized is a 401: the authenticator does not know the token yet.
	probeUnauthorized
	// probeReady is a 2xx SelfSubjectReview naming the user, or a 403 whose
	// body is a Kubernetes Status with reason Forbidden: the API server only
	// authorizes after it has authenticated.
	probeReady
)

// probeDirect posts one SelfSubjectReview with bearer to server.
func probeDirect(ctx context.Context, client *http.Client, server, bearer string) probeAnswer {
	req, err := kube.NewWriteRequest(ctx, http.MethodPost, strings.TrimSuffix(server, "/")+"/apis/authentication.k8s.io/v1/selfsubjectreviews", strings.NewReader(selfSubjectReviewBody))
	if err != nil {
		return probeRetry
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return probeRetry
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return probeUnauthorized
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var review struct {
			Status struct {
				UserInfo struct {
					Username string `json:"username"`
				} `json:"userInfo"`
			} `json:"status"`
		}
		if json.Unmarshal(body, &review) == nil && review.Status.UserInfo.Username != "" {
			return probeReady
		}
	case resp.StatusCode == http.StatusForbidden:
		var status struct {
			Kind       string `json:"kind"`
			APIVersion string `json:"apiVersion"`
			Reason     string `json:"reason"`
			Code       int    `json:"code"`
		}
		if json.Unmarshal(body, &status) == nil && status.Kind == "Status" && status.APIVersion == "v1" && status.Reason == "Forbidden" && status.Code == http.StatusForbidden {
			return probeReady
		}
	}
	return probeRetry
}

// directProbeUnavailable reports an endpoint that cannot be probed at all:
// its cluster entry does not yield a transport (an unparsable CA, say).
// The caller returns the credential without waiting and says so.
type directProbeUnavailable struct{ err error }

func (e *directProbeUnavailable) Error() string { return e.err.Error() }
func (e *directProbeUnavailable) Unwrap() error { return e.err }

// directClientFactory builds the HTTP client the direct-endpoint probe
// uses from the exec cluster's rest.Config. Production passes
// kube.HTTPClient; tests pass a client bound to an in-memory server.
type directClientFactory func(*rest.Config) (*http.Client, error)

// waitDirectReady probes ec until it authenticates bearer, for up to
// bound: every request derives from one deadline so a stalled dial cannot
// outlive it. It returns (true, nil) once a ready answer is followed by a
// settling interval without a 401, (false, nil) when the bound expires, and
// (false, ctx.Err()) when the caller cancels; the caller tells the last two
// apart by the error. newClient builds the probe's client from ec.cfg.
func waitDirectReady(ctx context.Context, ec *execCluster, bearer string, bound time.Duration, newClient directClientFactory) (bool, error) {
	if directWaitStarted != nil {
		directWaitStarted()
	}
	// The caller's cancellation wins over every other outcome, including
	// an endpoint that cannot be probed: a cancelled invocation revokes
	// its token, it never returns one.
	if err := ctx.Err(); err != nil {
		return false, err
	}
	client, err := newClient(ec.cfg)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return false, cerr
		}
		return false, &directProbeUnavailable{err}
	}
	wctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	ticker := time.NewTicker(directProbeInterval)
	defer ticker.Stop()
	var settleStart time.Time
	for {
		switch probeDirect(wctx, client, ec.server, bearer) {
		case probeReady:
			if settleStart.IsZero() {
				settleStart = time.Now()
			} else if time.Since(settleStart) >= directSettle {
				return true, nil
			}
		case probeUnauthorized:
			settleStart = time.Time{}
		}
		select {
		case <-ticker.C:
		case <-wctx.Done():
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, nil
		}
	}
}

// freshToken is a cluster token this invocation just created, with the
// capability to revoke it: the child of a stored or plugin-created login
// is deleted with its parent bearer whatever the parent's provenance, and
// the token of an interactive sign-in is deleted with its own bearer, which
// succeeds for an ext token and is refused for a cluster-scoped v3 token
// (that delete is reported failed).
type freshToken struct {
	create  CreateResult
	cleanup func(ctx context.Context) RevokeOutcome
}

// leftover says how the token ends when its delete fails, for the person
// who can act on it: the token, who can delete it, the command that person
// runs and, when the create reported one, the expiry. It is empty when the
// create named no token, since a delete is attempted only for a named one.
func (f *freshToken) leftover() string {
	c := f.create
	if c.Ref == nil {
		return ""
	}
	return signInLeftover(clierr.TokenFate{Kind: c.Ref.Kind, Ref: c.Ref, Marked: c.Marked, ExpiresAt: c.ExpiresAt}, c.UserID, "")
}

// newFreshToken pairs a successful create with the bearer that may delete
// it. An unknown or unidentified create has nothing to revoke.
func newFreshToken(created CreateResult, client *http.Client, base, bearer string) *freshToken {
	return &freshToken{create: created, cleanup: func(ctx context.Context) RevokeOutcome {
		if created.Outcome != CreateKnown || created.Ref == nil {
			return RevokeOutcome{Result: ResultNotAttempted}
		}
		return revokeToken(ctx, client, base, *created.Ref, bearer)
	}}
}

// directWaitDetails is the structured detail of a cancelled wait: what
// happened to the token the invocation had already created.
type directWaitDetails struct {
	Fate *clierr.TokenFate `json:"clusterToken,omitempty"`
	// unscoped says the token is for Rancher itself, which the text calls a
	// token, not a cluster token.
	unscoped bool
}

func (d directWaitDetails) TextDetails() []clierr.DetailLine {
	if d.Fate == nil || d.Fate.Cleanup == nil {
		return nil
	}
	parts := []clierr.DetailPart{clierr.ValuePart(clusterTokenNoun(!d.unscoped)+" cleanup: ", string(d.Fate.Cleanup.Result))}
	if d.Fate.Ref != nil {
		parts = append(parts, clierr.TokenRefParts(" (", *d.Fate.Ref)...)
		parts = append(parts, clierr.LabelPart(")"))
	}
	if d.Fate.Cleanup.Error != "" {
		parts = append(parts, clierr.ValuePart(": ", d.Fate.Cleanup.Error))
	}
	return []clierr.DetailLine{clierr.Line(parts...)}
}

// directWaitCancelled is the error for a caller cancellation during the
// wait: the fresh token's fate travels as details, the cancellation as the
// cause. A failed delete ends with the token's leftover text, which names
// the token, who can delete it and the command.
func directWaitCancelled(cause error, fresh *freshToken, cleanup RevokeOutcome) error {
	msg := "cancelled while waiting for the direct endpoint to accept the new credential"
	switch cleanup.Result {
	case ResultRevoked, ResultAlreadyGone:
		msg += "; the token it created was revoked"
	case ResultNotAttempted:
		msg += "; the create outcome is unknown, nothing was revoked"
	default:
		msg += "; the token it created could not be revoked"
		if leftover := fresh.leftover(); leftover != "" {
			msg += "; " + leftover
		}
	}
	return clierr.New(clierr.CodeGeneric, msg).WithCause(cause).WithDetails(fateDetails(fresh, cleanup))
}

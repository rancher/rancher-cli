// Package oidcclient holds the curated commands for Rancher's embedded OIDC
// provider (management.cattle.io/v3 OIDCClient, feature "oidc-provider"):
// create oidc-client and the oidc-client secret-lifecycle verb group
// (add-secret, rotate-secret, remove-secret). The server mechanism is
// annotation-driven - these commands exist so the Rancherism never leaks
// into the CLI's interface.
package oidcclient

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// oidcClientKind returns the oidcclient kind row. Every bespoke command in
// this package derives its Capability requirement, GVR, and Sensitive flag
// from this row; a silently-returned zero-value kind on a missing/renamed
// row would silently weaken the Prime gate every one of them shares, so a
// lookup failure here is a programmer error, not a runtime condition to
// recover from - "a command cannot weaken its own gates" (AGENTS.md) demands
// a loud failure.
func oidcClientKind() kinds.Kind {
	k, ok := kinds.Lookup("oidcclient")
	if !ok {
		panic("oidcclient kind row not found in kinds.go - renamed or removed without updating internal/engine/oidcclient")
	}
	return k
}

// capability returns the oidcclient kind row's Capability requirement - the
// ONE shared helper every bespoke descriptor in this package copies from,
// never a second hand-authored value, so the four bespoke commands and the
// generated verbs can never drift apart on their Capability.
func capability() runtime.CapabilityRequirement {
	return oidcClientKind().Capability
}

// secretGVR/secretNamespace name the backing Secret every client secret
// lives in: cattle-oidc-client-secrets/<clientID>, one data key per secret
// slot name (client-secret-1, client-secret-2, ...).
var secretGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

const secretNamespace = "cattle-oidc-client-secrets"

// readClientSecret GETs the backing Secret for clientID through the
// caller-credentialed local-cluster client - the SAME identity path
// `get secret --cluster local` uses, so a caller never has more rights
// here than they would through the generic verb - selects the exact data
// key, and decodes it with standard base64 (the dynamic client exposes
// Secret data as base64 strings; only decoded plaintext may ever reach a
// clientSecret field). It is the ONE shared read path: create's delivery
// read, add-secret's Plan-time preflight (GET-and-discard), and rotate-
// secret's Plan-time preflight/correlation read all call this, never a
// second hand-authored GET-and-decode.
//
// A missing key or malformed base64 is returned as a plain structured
// error: the caller classifies it - a post-mutation caller wraps it as a
// non-retryable committed-partial-success error (clierr.CommittedContextDetails);
// a Plan-time (pre-mutation) caller returns it as-is, a structured
// server-data error with no committed mutation to report.
func readClientSecret(ctx context.Context, clients engine.ClusterClientFactory, clientID, key string) (string, error) {
	client, err := clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return "", err
	}
	obj, err := client.Get(ctx, secretGVR, secretNamespace, clientID)
	if err != nil {
		return "", err
	}
	encoded, found, _ := unstructured.NestedString(obj.Object, "data", key)
	if !found || encoded == "" {
		return "", clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("Secret %s/%s has no data key %q", secretNamespace, clientID, key))
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("Secret %s/%s data key %q is not valid base64: %v", secretNamespace, clientID, key, err))
	}
	return string(decoded), nil
}

// recoveryHint builds the standard non-mutating re-check command every
// oidc-client verb points a caller at after a post-commit or Plan-time
// refusal: "get oidcclient <name> -o json". The ONE shared spelling -
// never a second hand-authored copy - so a future wording change (e.g.
// adding --cluster local) only has to be made here.
func recoveryHint(name string) string {
	return fmt.Sprintf("get oidcclient %s -o json", name)
}

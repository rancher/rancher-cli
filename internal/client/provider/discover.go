package provider

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
)

// serverVersion reads the /version document through disc's REST client
// with ctx on the request. The discovery client's own ServerVersion() takes
// no context, so a request made through it outlives the caller's
// cancellation and ends only with the request timeout.
func serverVersion(ctx context.Context, disc discovery.DiscoveryInterface) (*version.Info, error) {
	body, err := disc.RESTClient().Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil {
		return nil, restoreStatus(err, body)
	}
	var info version.Info
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("unable to parse the server version: %w", err)
	}
	return &info, nil
}

// serverGroups reads the /apis document (every named API group with its
// preferred version) through disc's REST client with ctx on the request,
// for the same reason as serverVersion: ServerGroups() takes no context.
// The core group is not in this document and never needs it - no kind row
// leaves a core version empty.
func serverGroups(ctx context.Context, disc discovery.DiscoveryInterface) (*metav1.APIGroupList, error) {
	body, err := disc.RESTClient().Get().AbsPath("/apis").Do(ctx).Raw()
	if err != nil {
		return nil, restoreStatus(err, body)
	}
	var groups metav1.APIGroupList
	if err := json.Unmarshal(body, &groups); err != nil {
		return nil, fmt.Errorf("unable to parse the server's API groups: %w", err)
	}
	return &groups, nil
}

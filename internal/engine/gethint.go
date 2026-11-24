package engine

import (
	"errors"

	"github.com/rancher/rancher-cli/internal/clierr"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// WithMissingGetHint adds a hint to the Kubernetes authorizer's refusal of the
// GET that a command sends before it writes. The GET is for the object called
// name of resource gvr in namespace ns. The hint names the missing verb: "this
// <command> reads the <kind> first; your role needs permission to get
// <resource>", followed by " as well as <verb>" when verb is not "". verb is
// the verb of the write when the command writes the object it reads; "" when
// the write can go to another object. The resource has its group (configmaps,
// clusters.provisioning.cattle.io).
// Any other error, and one that already has a hint, is returned as it is.
func WithMissingGetHint(err error, command, kind string, gvr schema.GroupVersionResource, ns, name, verb string) error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok || ce.Hint != "" || !clierr.AuthorizerRefused(err, "get", gvr.Group, gvr.Resource, ns, name) {
		return err
	}
	resource := gvr.Resource
	if gvr.Group != "" {
		resource += "." + gvr.Group
	}
	hint := "this " + command + " reads the " + kind + " first; your role needs permission to get " + resource
	if verb != "" {
		hint += " as well as " + verb
	}
	return ce.WithMessage(ce.Message).WithHint(hint)
}

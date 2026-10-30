package plaintext

import "github.com/rancher/rancher-cli/internal/runtime"

// Other has a field named PlainText that is not the field of runtime.Descriptor.
type Other struct{ PlainText bool }

func literalKey() runtime.Descriptor { return runtime.Descriptor{PlainText: true} }

func assign(d *runtime.Descriptor) { d.PlainText = true }

func addressTaken(d *runtime.Descriptor) *bool { return &d.PlainText }

func read(d runtime.Descriptor) bool { return d.PlainText }

func otherType(o Other) bool { return o.PlainText }

func otherAssign(o *Other) { o.PlainText = true }

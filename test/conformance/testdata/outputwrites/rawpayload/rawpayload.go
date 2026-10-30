package rawpayload

import "github.com/rancher/rancher-cli/internal/runtime"

// Other has fields named Raw and RawBytes that are not the fields of runtime.Result.
type Other struct {
	Raw      bool
	RawBytes []byte
}

func bytesLiteralKey(b []byte) runtime.Result { return runtime.Result{RawBytes: b} }

func bytesAssign(res *runtime.Result, b []byte) { res.RawBytes = b }

func bytesAddress(res *runtime.Result) *[]byte { return &res.RawBytes }

func rawLiteralKey() runtime.Result { return runtime.Result{Raw: true} }

func rawAssign(res *runtime.Result) { res.Raw = true }

func read(res runtime.Result) bool { return res.Raw }

func otherWrites(o *Other, b []byte) Other {
	o.Raw = true
	o.RawBytes = b
	return Other{Raw: true, RawBytes: b}
}

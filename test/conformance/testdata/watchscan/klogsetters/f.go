package klogsetters

import (
	"flag"
	"io"

	"k8s.io/klog/v2"
)

func configure() {
	klog.SetOutput(io.Discard)
	klog.InitFlags(nil)
	_ = flag.Set("v", "3")
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	klog.InitFlags(fs)
}

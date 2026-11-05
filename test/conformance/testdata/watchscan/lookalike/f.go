package lookalike

import (
	"flag"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// watcher has a method named Watch that is not client-go's.
type watcher struct{}

func (watcher) Watch() {}

func quiet(w watcher) {
	w.Watch()
	_ = metav1.ListOptions{Watch: false}
	var o metav1.ListOptions
	o.Watch = false
	_ = o
	_ = "watch=false and the word watch"
	_ = flag.Set("other", "x")
	klog.Info("logging is not configuring")
	_ = klog.V(1)
}

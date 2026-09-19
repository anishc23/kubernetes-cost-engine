package api

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// runtimeObject lets the tests build a fake cluster from a mixed slice without
// importing runtime into every test function.
type runtimeObject = runtime.Object

func toRuntime(objs []runtimeObject) []runtime.Object { return objs }

func ns(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

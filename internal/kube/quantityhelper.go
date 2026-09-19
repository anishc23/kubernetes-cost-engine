package kube

import "k8s.io/apimachinery/pkg/api/resource"

func mustQuantity(s string) resource.Quantity {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		panic(err)
	}
	return q
}

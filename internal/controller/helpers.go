package controller

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

func sameTimePtr(a, b *metav1.Time) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Time.Equal(b.Time)
}

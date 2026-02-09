package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
)

type NetworkGrantReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *NetworkGrantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var grant workspacesv1alpha1.NetworkGrant
	if err := r.Get(ctx, req.NamespacedName, &grant); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	cnpName := fmt.Sprintf("netgrant-%s", grant.Name)
	cnp := &unstructured.Unstructured{}
	cnp.SetAPIVersion("cilium.io/v2")
	cnp.SetKind("CiliumNetworkPolicy")
	cnp.SetNamespace(grant.Namespace)
	cnp.SetName(cnpName)

	// If not approved, ensure policy doesn't exist.
	if !grant.Spec.Approved {
		_ = r.Delete(ctx, cnp)
		if grant.Status.Active {
			patch := client.MergeFrom(grant.DeepCopy())
			grant.Status.Active = false
			_ = r.Status().Patch(ctx, &grant, patch)
		}
		return ctrl.Result{}, nil
	}

	ttl := grant.Spec.TTLSeconds
	if ttl <= 0 {
		ttl = 1800
	}

	now := time.Now()
	expiresAt := grant.Status.ExpiresAt.Time
	if expiresAt.IsZero() {
		expiresAt = now.Add(time.Duration(ttl) * time.Second)
		patch := client.MergeFrom(grant.DeepCopy())
		grant.Status.ExpiresAt = metav1.NewTime(expiresAt)
		_ = r.Status().Patch(ctx, &grant, patch)
	}

	if now.After(expiresAt) {
		_ = r.Delete(ctx, cnp)
		if grant.Status.Active {
			patch := client.MergeFrom(grant.DeepCopy())
			grant.Status.Active = false
			_ = r.Status().Patch(ctx, &grant, patch)
		}
		return ctrl.Result{}, nil
	}

	if len(grant.Spec.PodSelector.MatchExpressions) != 0 {
		// MVP: keep it simple; require matchLabels only.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	matchLabels := grant.Spec.PodSelector.MatchLabels
	if len(matchLabels) == 0 {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	spec := map[string]any{
		"endpointSelector": map[string]any{
			"matchLabels": matchLabels,
		},
		"egress": buildCiliumEgress(grant.Spec.Egress),
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cnp, func() error {
		if err := unstructured.SetNestedField(cnp.Object, spec, "spec"); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(&grant, cnp, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	if !grant.Status.Active {
		patch := client.MergeFrom(grant.DeepCopy())
		grant.Status.Active = true
		_ = r.Status().Patch(ctx, &grant, patch)
	}

	// Requeue at expiry boundary.
	return ctrl.Result{RequeueAfter: time.Until(expiresAt)}, nil
}

func (r *NetworkGrantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacesv1alpha1.NetworkGrant{}).
		Complete(r)
}

func buildCiliumEgress(rules []workspacesv1alpha1.NetworkGrantEgressRule) []any {
	out := make([]any, 0, len(rules))
	for _, r := range rules {
		ports := r.Ports
		if len(ports) == 0 {
			ports = []int32{443}
		}
		ps := make([]any, 0, len(ports))
		for _, p := range ports {
			ps = append(ps, map[string]any{"port": fmt.Sprintf("%d", p), "protocol": "TCP"})
		}
		out = append(out, map[string]any{
			"toFQDNs": []any{map[string]any{"matchName": r.Host}},
			"toPorts": []any{map[string]any{"ports": ps}},
		})
	}
	return out
}

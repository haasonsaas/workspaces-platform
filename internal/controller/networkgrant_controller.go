package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

const networkGrantCondSpecValid = "SpecValid"

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

	// Validate spec. Without an admission webhook, the controller must ensure
	// invalid requests never produce enforceable network policy.
	matchLabels, err := validateAndResolveNetworkGrantMatchLabels(&grant)
	if err != nil {
		_ = r.Delete(ctx, cnp)
		if grant.Status.Active {
			patch := client.MergeFrom(grant.DeepCopy())
			grant.Status.Active = false
			_ = r.Status().Patch(ctx, &grant, patch)
		}
		_ = r.setNetworkGrantSpecValidCondition(ctx, &grant, false, "ValidationFailed", err.Error())
		return ctrl.Result{}, nil
	}
	_ = r.setNetworkGrantSpecValidCondition(ctx, &grant, true, "Valid", "spec is valid")

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

	spec := map[string]any{
		"endpointSelector": map[string]any{
			"matchLabels": matchLabels,
		},
		"egress": buildCiliumEgress(grant.Spec.Egress),
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, cnp, func() error {
		if err := unstructured.SetNestedField(cnp.Object, spec, "spec"); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(&grant, cnp, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	_ = op

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

func validateAndResolveNetworkGrantMatchLabels(grant *workspacesv1alpha1.NetworkGrant) (map[string]string, error) {
	// Selector must be explicit and stable.
	var matchLabels map[string]string
	if grant.Spec.AgentJobRef != nil && strings.TrimSpace(grant.Spec.AgentJobRef.Name) != "" {
		if grant.Spec.PodSelector != nil {
			return nil, fmt.Errorf("podSelector is not allowed when agentJobRef is set")
		}
		matchLabels = map[string]string{
			labelApp:      "agent",
			labelAgentJob: strings.TrimSpace(grant.Spec.AgentJobRef.Name),
		}
	} else {
		if grant.Spec.PodSelector == nil {
			return nil, fmt.Errorf("agentJobRef or podSelector is required")
		}
		if len(grant.Spec.PodSelector.MatchExpressions) != 0 {
			return nil, fmt.Errorf("podSelector.matchExpressions not supported; use matchLabels only")
		}
		if len(grant.Spec.PodSelector.MatchLabels) == 0 {
			return nil, fmt.Errorf("podSelector.matchLabels is required")
		}
		matchLabels = grant.Spec.PodSelector.MatchLabels
	}

	purpose := strings.TrimSpace(grant.Spec.Purpose)
	if purpose == "" {
		return nil, fmt.Errorf("purpose is required")
	}

	if grant.Spec.Approved && strings.TrimSpace(grant.Spec.ApprovedBy) == "" {
		return nil, fmt.Errorf("approvedBy is required when approved is true")
	}

	mode := grant.Spec.PolicyMode
	if mode == "" {
		mode = workspacesv1alpha1.NetworkGrantPolicyModeStrictFQDN
	}
	if mode != workspacesv1alpha1.NetworkGrantPolicyModeStrictFQDN {
		return nil, fmt.Errorf("policyMode %q is not supported (MVP supports STRICT_FQDN only)", mode)
	}

	proto := grant.Spec.Protocol
	if proto == "" {
		proto = workspacesv1alpha1.NetworkGrantProtocolTCP
	}
	if proto != workspacesv1alpha1.NetworkGrantProtocolTCP {
		return nil, fmt.Errorf("protocol %q is not supported (MVP supports TCP only)", proto)
	}

	if len(grant.Spec.Egress) == 0 {
		return nil, fmt.Errorf("egress must contain at least one destination")
	}

	for i, r := range grant.Spec.Egress {
		host := strings.TrimSpace(r.Host)
		if host == "" {
			return nil, fmt.Errorf("egress[%d].host is required", i)
		}
		if strings.ContainsAny(host, " \t\r\n") {
			return nil, fmt.Errorf("egress[%d].host must not contain whitespace", i)
		}
		if strings.Contains(host, "*") {
			return nil, fmt.Errorf("egress[%d].host must be an exact FQDN (no wildcards)", i)
		}
		if strings.ContainsAny(host, "/:") {
			return nil, fmt.Errorf("egress[%d].host must be a hostname only (no scheme/path/port)", i)
		}
		if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
			return nil, fmt.Errorf("egress[%d].host must not start or end with '.'", i)
		}
		if len(host) > 253 {
			return nil, fmt.Errorf("egress[%d].host is too long", i)
		}

		ports := r.Ports
		if len(ports) == 0 {
			ports = []int32{443}
		}
		for _, p := range ports {
			if p <= 0 || p > 65535 {
				return nil, fmt.Errorf("egress[%d] has invalid port %d", i, p)
			}
			if !grant.Spec.AllowNon443 && p != 443 {
				return nil, fmt.Errorf("egress[%d] requests non-443 port %d but allowNon443 is false", i, p)
			}
		}
	}

	return matchLabels, nil
}

func buildCiliumEgress(rules []workspacesv1alpha1.NetworkGrantEgressRule) []any {
	out := make([]any, 0, len(rules))
	for _, r := range rules {
		// Validation ensures host is already exact; keep a canonical lower-case matchName.
		host := strings.ToLower(strings.TrimSpace(r.Host))
		ports := r.Ports
		if len(ports) == 0 {
			ports = []int32{443}
		}
		ps := make([]any, 0, len(ports))
		for _, p := range ports {
			ps = append(ps, map[string]any{"port": fmt.Sprintf("%d", p), "protocol": "TCP"})
		}
		out = append(out, map[string]any{
			"toFQDNs": []any{map[string]any{"matchName": host}},
			"toPorts": []any{map[string]any{"ports": ps}},
		})
	}
	return out
}

func (r *NetworkGrantReconciler) setNetworkGrantSpecValidCondition(ctx context.Context, grant *workspacesv1alpha1.NetworkGrant, ok bool, reason, message string) error {
	condStatus := metav1.ConditionFalse
	if ok {
		condStatus = metav1.ConditionTrue
	}

	desired := metav1.Condition{
		Type:               networkGrantCondSpecValid,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: grant.GetGeneration(),
		LastTransitionTime: metav1.Now(),
	}

	before := grant.DeepCopy()
	meta.SetStatusCondition(&grant.Status.Conditions, desired)
	if reflect.DeepEqual(before.Status.Conditions, grant.Status.Conditions) {
		return nil
	}

	patch := client.MergeFrom(before)
	return r.Status().Patch(ctx, grant, patch)
}

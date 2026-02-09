package controller

import (
	"context"
	"fmt"
	"reflect"
	"sort"
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
	"workspaces-platform/internal/netutil"
)

type NetworkGrantReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// MaxTTLSeconds caps how long approved grants can remain active. 0 disables the cap.
	MaxTTLSeconds int32

	// MaxEgressRules caps the number of destinations per grant. 0 disables the cap.
	MaxEgressRules int

	// MaxDNSNames caps the number of unique DNS names allowed for DNS L7 allow rules
	// (union of spec.egress hosts and spec.dnsAllow). 0 disables the cap.
	MaxDNSNames int
}

const networkGrantCondSpecValid = "SpecValid"
const networkGrantDefaultTTLSeconds int32 = 1800

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
	matchLabels, dnsNames, err := validateAndResolveNetworkGrantMatchLabels(&grant, r.MaxTTLSeconds, r.MaxEgressRules, r.MaxDNSNames)
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
		if grant.Status.Active || !grant.Status.ExpiresAt.IsZero() || !grant.Status.ApprovedAt.IsZero() {
			patch := client.MergeFrom(grant.DeepCopy())
			grant.Status.Active = false
			grant.Status.ExpiresAt = metav1.Time{}
			grant.Status.ApprovedAt = metav1.Time{}
			_ = r.Status().Patch(ctx, &grant, patch)
		}
		return ctrl.Result{}, nil
	}

	now := time.Now()
	ttl, err := resolveNetworkGrantTTLSeconds(grant.Spec.TTLSeconds, r.MaxTTLSeconds)
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

	approvedAt := grant.Status.ApprovedAt.Time
	if approvedAt.IsZero() {
		before := grant.DeepCopy()
		grant.Status.ApprovedAt = metav1.NewTime(now)
		patch := client.MergeFrom(before)
		_ = r.Status().Patch(ctx, &grant, patch)
		approvedAt = now
	}

	desiredExpiresAt := approvedAt.Add(time.Duration(ttl) * time.Second)
	expiresAt := grant.Status.ExpiresAt.Time
	if expiresAt.IsZero() || expiresAt.After(desiredExpiresAt) {
		before := grant.DeepCopy()
		grant.Status.ExpiresAt = metav1.NewTime(desiredExpiresAt)
		patch := client.MergeFrom(before)
		_ = r.Status().Patch(ctx, &grant, patch)
		expiresAt = desiredExpiresAt
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

	mode := grant.Spec.PolicyMode
	if mode == "" {
		mode = workspacesv1alpha1.NetworkGrantPolicyModeStrictFQDN
	}

	// PROXY_CONNECT mode: do not create direct egress policy. The egress-proxy
	// enforces the destination allowlist using this NetworkGrant. We still create
	// DNS L7 allow rules for the approved hostnames so tools that resolve before
	// CONNECT continue to work without broad DNS exfil surface.
	if mode == workspacesv1alpha1.NetworkGrantPolicyModeProxyConnect {
		egress := []any{}
		if dnsRule := buildCiliumDNSAllowEgress(dnsNames); dnsRule != nil {
			egress = []any{dnsRule}
		}
		spec := map[string]any{
			"endpointSelector": map[string]any{
				"matchLabels": matchLabels,
			},
			"egress": egress,
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
		return ctrl.Result{RequeueAfter: time.Until(expiresAt)}, nil
	}

	spec := map[string]any{
		"endpointSelector": map[string]any{
			"matchLabels": matchLabels,
		},
		"egress": buildCiliumNetworkGrantEgress(grant.Spec.Egress, dnsNames),
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

func resolveNetworkGrantTTLSeconds(requested, max int32) (int32, error) {
	if requested > 0 {
		if max > 0 && requested > max {
			return 0, fmt.Errorf("ttlSeconds %d exceeds max %d", requested, max)
		}
		return requested, nil
	}

	ttl := networkGrantDefaultTTLSeconds
	if max > 0 && ttl > max {
		ttl = max
	}
	return ttl, nil
}

func validateAndResolveNetworkGrantMatchLabels(grant *workspacesv1alpha1.NetworkGrant, maxTTLSeconds int32, maxEgressRules int, maxDNSNames int) (map[string]string, []string, error) {
	// Selector must be explicit and stable.
	var matchLabels map[string]string
	if grant.Spec.AgentJobRef != nil && strings.TrimSpace(grant.Spec.AgentJobRef.Name) != "" {
		if grant.Spec.PodSelector != nil {
			return nil, nil, fmt.Errorf("podSelector is not allowed when agentJobRef is set")
		}
		matchLabels = map[string]string{
			labelApp:      "agent",
			labelAgentJob: strings.TrimSpace(grant.Spec.AgentJobRef.Name),
		}
	} else {
		if grant.Spec.PodSelector == nil {
			return nil, nil, fmt.Errorf("agentJobRef or podSelector is required")
		}
		if len(grant.Spec.PodSelector.MatchExpressions) != 0 {
			return nil, nil, fmt.Errorf("podSelector.matchExpressions not supported; use matchLabels only")
		}
		if len(grant.Spec.PodSelector.MatchLabels) == 0 {
			return nil, nil, fmt.Errorf("podSelector.matchLabels is required")
		}
		matchLabels = grant.Spec.PodSelector.MatchLabels
	}

	purpose := strings.TrimSpace(grant.Spec.Purpose)
	if purpose == "" {
		return nil, nil, fmt.Errorf("purpose is required")
	}

	if grant.Spec.Approved && strings.TrimSpace(grant.Spec.ApprovedBy) == "" {
		return nil, nil, fmt.Errorf("approvedBy is required when approved is true")
	}

	mode := grant.Spec.PolicyMode
	if mode == "" {
		mode = workspacesv1alpha1.NetworkGrantPolicyModeStrictFQDN
	}
	switch mode {
	case workspacesv1alpha1.NetworkGrantPolicyModeStrictFQDN, workspacesv1alpha1.NetworkGrantPolicyModeProxyConnect:
	default:
		return nil, nil, fmt.Errorf("policyMode %q is not supported", mode)
	}

	proto := grant.Spec.Protocol
	if proto == "" {
		proto = workspacesv1alpha1.NetworkGrantProtocolTCP
	}
	if proto != workspacesv1alpha1.NetworkGrantProtocolTCP {
		return nil, nil, fmt.Errorf("protocol %q is not supported (MVP supports TCP only)", proto)
	}

	if len(grant.Spec.Egress) == 0 {
		return nil, nil, fmt.Errorf("egress must contain at least one destination")
	}
	if maxEgressRules > 0 && len(grant.Spec.Egress) > maxEgressRules {
		return nil, nil, fmt.Errorf("egress contains too many destinations (%d > %d)", len(grant.Spec.Egress), maxEgressRules)
	}

	if _, err := resolveNetworkGrantTTLSeconds(grant.Spec.TTLSeconds, maxTTLSeconds); err != nil {
		return nil, nil, err
	}

	dnsSet := map[string]struct{}{}

	for i, r := range grant.Spec.Egress {
		host := strings.TrimSpace(r.Host)
		if err := validateNetworkGrantHost(host); err != nil {
			return nil, nil, fmt.Errorf("egress[%d].host %q is invalid: %w", i, host, err)
		}

		dnsSet[strings.ToLower(host)] = struct{}{}

		ports := r.Ports
		if len(ports) == 0 {
			ports = []int32{443}
		}
		for _, p := range ports {
			if p <= 0 || p > 65535 {
				return nil, nil, fmt.Errorf("egress[%d] has invalid port %d", i, p)
			}
			if !grant.Spec.AllowNon443 && p != 443 {
				return nil, nil, fmt.Errorf("egress[%d] requests non-443 port %d but allowNon443 is false", i, p)
			}
		}
	}

	for i, host := range grant.Spec.DNSAllow {
		h := strings.TrimSpace(host)
		if h == "" {
			return nil, nil, fmt.Errorf("dnsAllow[%d] is empty", i)
		}
		if err := validateNetworkGrantHost(h); err != nil {
			return nil, nil, fmt.Errorf("dnsAllow[%d] %q is invalid: %w", i, h, err)
		}
		dnsSet[strings.ToLower(h)] = struct{}{}
	}

	dnsNames := make([]string, 0, len(dnsSet))
	for h := range dnsSet {
		dnsNames = append(dnsNames, h)
	}
	sort.Strings(dnsNames)

	if maxDNSNames > 0 && len(dnsNames) > maxDNSNames {
		return nil, nil, fmt.Errorf("too many DNS names for allow rules (%d > %d)", len(dnsNames), maxDNSNames)
	}

	return matchLabels, dnsNames, nil
}

func validateNetworkGrantHost(host string) error {
	return netutil.ValidateExactHostname(host)
}

func buildCiliumNetworkGrantEgress(rules []workspacesv1alpha1.NetworkGrantEgressRule, dnsNames []string) []any {
	out := buildCiliumEgress(rules)
	if dnsRule := buildCiliumDNSAllowEgress(dnsNames); dnsRule != nil {
		out = append([]any{dnsRule}, out...)
	}
	return out
}

func buildCiliumDNSAllowEgress(dnsNames []string) any {
	if len(dnsNames) == 0 {
		return nil
	}

	dnsRules := make([]any, 0, len(dnsNames))
	for _, h := range dnsNames {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		dnsRules = append(dnsRules, map[string]any{"matchName": h})
	}
	if len(dnsRules) == 0 {
		return nil
	}

	// Allow DNS queries for the requested names via kube-dns/CoreDNS. This is
	// additive with baseline policy (which should default-deny external DNS).
	return map[string]any{
		"toEndpoints": []any{
			map[string]any{
				"matchLabels": map[string]string{
					"k8s:io.kubernetes.pod.namespace": "kube-system",
					"k8s-app":                         "kube-dns",
				},
			},
		},
		"toPorts": []any{
			map[string]any{
				"ports": []any{
					map[string]any{"port": "53", "protocol": "UDP"},
					map[string]any{"port": "53", "protocol": "TCP"},
				},
				"rules": map[string]any{
					"dns": dnsRules,
				},
			},
		},
	}
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

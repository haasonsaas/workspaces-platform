package controller

import (
	"context"
	"fmt"
	"hash/fnv"
	"reflect"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
)

type PortShareReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const portShareCondSpecValid = "SpecValid"

func (r *PortShareReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ps workspacesv1alpha1.PortShare
	if err := r.Get(ctx, req.NamespacedName, &ps); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if ps.Spec.DesktopRef.Name == "" || ps.Spec.Port <= 0 {
		_ = r.setPortShareSpecValidCondition(ctx, &ps, false, "InvalidSpec", "desktopRef.name and port are required")
		return ctrl.Result{}, nil
	}
	_ = r.setPortShareSpecValidCondition(ctx, &ps, true, "Valid", "spec is valid")

	var desk workspacesv1alpha1.Desktop
	if err := r.Get(ctx, client.ObjectKey{Namespace: ps.Namespace, Name: strings.TrimSpace(ps.Spec.DesktopRef.Name)}, &desk); err != nil {
		if apierrors.IsNotFound(err) {
			// The target Desktop may be created later.
			_ = r.setPortShareSpecValidCondition(ctx, &ps, false, "DesktopNotFound", "referenced Desktop does not exist")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// TTL semantics: if ttlSeconds is set, enforce expiry by deleting the backing
	// Service on expiry (the PortShare object remains as record).
	now := time.Now().UTC()
	exp := ps.Status.ExpiresAt.Time
	if ps.Spec.TTLSeconds > 0 {
		start := ps.CreationTimestamp.Time
		if ps.Spec.RequestedAt != nil && !ps.Spec.RequestedAt.Time.IsZero() {
			start = ps.Spec.RequestedAt.Time
		}
		if start.IsZero() {
			start = now
		}

		desired := start.Add(time.Duration(ps.Spec.TTLSeconds) * time.Second)
		if exp.IsZero() || !exp.Equal(desired) {
			before := ps.DeepCopy()
			ps.Status.ExpiresAt = metav1.NewTime(desired)
			_ = r.Status().Patch(ctx, &ps, client.MergeFrom(before))
			exp = desired
		}
	} else if !ps.Status.ExpiresAt.IsZero() {
		before := ps.DeepCopy()
		ps.Status.ExpiresAt = metav1.Time{}
		_ = r.Status().Patch(ctx, &ps, client.MergeFrom(before))
		exp = time.Time{}
	}

	svcName := portShareServiceName(desk.Name, ps.Spec.Port)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: ps.Namespace}}

	if !exp.IsZero() && now.After(exp) {
		_ = r.Delete(ctx, svc)
		if ps.Status.Active || ps.Status.ServiceName != "" {
			before := ps.DeepCopy()
			ps.Status.Active = false
			ps.Status.ServiceName = ""
			_ = r.Status().Patch(ctx, &ps, client.MergeFrom(before))
		}
		return ctrl.Result{}, nil
	}

	labels := map[string]string{
		labelApp:       "desktop",
		labelDesktop:   desk.Name,
		labelPortShare: ps.Name,
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		for k, v := range labels {
			svc.Labels[k] = v
		}
		svc.Spec.Selector = map[string]string{
			labelApp:     "desktop",
			labelDesktop: desk.Name,
		}
		svc.Spec.Ports = []corev1.ServicePort{
			{
				Name:       "p" + strconv.Itoa(int(ps.Spec.Port)),
				Port:       ps.Spec.Port,
				TargetPort: intstrFromInt(int(ps.Spec.Port)),
				Protocol:   corev1.ProtocolTCP,
			},
		}
		return controllerutil.SetControllerReference(&ps, svc, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	if !ps.Status.Active || ps.Status.ServiceName != svcName {
		before := ps.DeepCopy()
		ps.Status.Active = true
		ps.Status.ServiceName = svcName
		_ = r.Status().Patch(ctx, &ps, client.MergeFrom(before))
	}

	if !exp.IsZero() {
		return ctrl.Result{RequeueAfter: time.Until(exp)}, nil
	}
	return ctrl.Result{}, nil
}

func (r *PortShareReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacesv1alpha1.PortShare{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

func portShareServiceName(desktopName string, port int32) string {
	desk := strings.ToLower(strings.TrimSpace(desktopName))
	if desk == "" {
		desk = "desktop"
	}
	base := fmt.Sprintf("desktop-%s-port-%d", desk, port)
	if len(base) <= 63 {
		return base
	}
	// Add a stable hash suffix to avoid collisions when truncating.
	h := fnv.New32a()
	_, _ = h.Write([]byte(desk))
	_, _ = h.Write([]byte(":"))
	_, _ = h.Write([]byte(strconv.FormatInt(int64(port), 10)))
	suf := fmt.Sprintf("-%08x", h.Sum32())
	maxPrefix := 63 - len(suf)
	if maxPrefix < 1 {
		return "ps" + suf[1:]
	}
	prefix := base[:maxPrefix]
	prefix = strings.TrimSuffix(prefix, "-")
	if prefix == "" {
		prefix = "ps"
	}
	return prefix + suf
}

func (r *PortShareReconciler) setPortShareSpecValidCondition(ctx context.Context, ps *workspacesv1alpha1.PortShare, status bool, reason, msg string) error {
	condStatus := metav1.ConditionFalse
	if status {
		condStatus = metav1.ConditionTrue
	}

	cond := metav1.Condition{
		Type:               portShareCondSpecValid,
		Status:             condStatus,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: ps.Generation,
		LastTransitionTime: metav1.Now(),
	}

	before := ps.DeepCopy()
	meta.SetStatusCondition(&ps.Status.Conditions, cond)
	if reflect.DeepEqual(before.Status.Conditions, ps.Status.Conditions) {
		return nil
	}
	return r.Status().Patch(ctx, ps, client.MergeFrom(before))
}

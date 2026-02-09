package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
)

type server struct {
	k8s client.Client

	adminToken string
}

func main() {
	var (
		listenAddr = getenv("LISTEN_ADDR", ":8080")
		adminToken = os.Getenv("BROKER_ADMIN_TOKEN")
	)

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(workspacesv1alpha1.AddToScheme(scheme))

	cfg := ctrl.GetConfigOrDie()
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("k8s client: %v", err)
	}

	s := &server{k8s: k8sClient, adminToken: adminToken}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	r.Route("/v1", func(r chi.Router) {
		r.Post("/network-grants", s.handleCreateNetworkGrant)
		r.Post("/network-grants/{namespace}/{name}/approve", s.handleApproveNetworkGrant)

		// MVP placeholder. The broker is the only component allowed to write to GitHub,
		// but wiring a full GitHub App flow is out-of-scope for the initial operator skeleton.
		r.Post("/github/open-pr", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotImplemented, map[string]any{
				"error": "not_implemented",
				"hint":  "Implement broker-only PR creation using a GitHub App installation token.",
			})
		})
	})

	log.Printf("capability-broker listening on %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, r))
}

type createNetworkGrantRequest struct {
	Namespace string `json:"namespace"`

	// Optional. If empty, server will generate a name.
	Name string `json:"name,omitempty"`

	PodSelector map[string]string `json:"podSelector"`

	Egress []workspacesv1alpha1.NetworkGrantEgressRule `json:"egress,omitempty"`

	TTLSeconds int32 `json:"ttlSeconds,omitempty"`

	Reason string `json:"reason,omitempty"`
}

func (s *server) handleCreateNetworkGrant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req createNetworkGrantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}
	if req.Namespace == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "namespace_required"})
		return
	}
	if len(req.PodSelector) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "podSelector_required"})
		return
	}
	if req.TTLSeconds == 0 {
		req.TTLSeconds = 1800
	}

	ng := &workspacesv1alpha1.NetworkGrant{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: req.Namespace,
		},
		Spec: workspacesv1alpha1.NetworkGrantSpec{
			PodSelector: metav1.LabelSelector{MatchLabels: req.PodSelector},
			Egress:      req.Egress,
			TTLSeconds:  req.TTLSeconds,
			Approved:    false,
			Reason:      req.Reason,
		},
	}
	if req.Name != "" {
		ng.Name = req.Name
	} else {
		ng.GenerateName = "netgrant-"
	}

	if err := s.k8s.Create(ctx, ng); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "create_failed"})
		return
	}

	writeJSON(w, http.StatusCreated, ng)
}

type approveNetworkGrantRequest struct {
	ApprovedBy string `json:"approvedBy"`
	Reason     string `json:"reason,omitempty"`
	TTLSeconds *int32 `json:"ttlSeconds,omitempty"`
}

func (s *server) handleApproveNetworkGrant(w http.ResponseWriter, r *http.Request) {
	if err := s.requireAdmin(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

	ctx := r.Context()
	ns := chi.URLParam(r, "namespace")
	name := chi.URLParam(r, "name")
	if ns == "" || name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "namespace_and_name_required"})
		return
	}

	var body approveNetworkGrantRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}
	if body.ApprovedBy == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "approvedBy_required"})
		return
	}

	var ng workspacesv1alpha1.NetworkGrant
	if err := s.k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &ng); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
		return
	}

	ngPatch := client.MergeFrom(ng.DeepCopy())
	ng.Spec.Approved = true
	ng.Spec.ApprovedBy = body.ApprovedBy
	if body.Reason != "" {
		ng.Spec.Reason = body.Reason
	}
	if body.TTLSeconds != nil {
		ng.Spec.TTLSeconds = *body.TTLSeconds
	}

	if err := s.k8s.Patch(ctx, &ng, ngPatch); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "approve_failed"})
		return
	}

	writeJSON(w, http.StatusOK, &ng)
}

func (s *server) requireAdmin(r *http.Request) error {
	if s.adminToken == "" {
		return errors.New("admin token not configured")
	}
	got := strings.TrimSpace(r.Header.Get("X-Broker-Admin-Token"))
	if got == "" || got != s.adminToken {
		return errors.New("invalid token")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

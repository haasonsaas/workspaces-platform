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
	auditpkg "workspaces-platform/internal/audit"
)

type server struct {
	k8s client.Client

	audit auditpkg.Emitter

	agentToken string
	adminToken string

	gh *githubService
}

func main() {
	var (
		listenAddr = getenv("LISTEN_ADDR", ":8080")
		agentToken = os.Getenv("BROKER_AGENT_TOKEN")
		adminToken = os.Getenv("BROKER_ADMIN_TOKEN")
	)

	auditEmitter, err := auditpkg.NewFromEnv("capability-broker")
	if err != nil {
		log.Fatalf("audit: %v", err)
	}
	defer func() { _ = auditEmitter.Close() }()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(workspacesv1alpha1.AddToScheme(scheme))

	cfg := ctrl.GetConfigOrDie()
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("k8s client: %v", err)
	}

	ghSvc, ghErr := newGitHubServiceFromEnv(auditEmitter)
	if ghErr != nil {
		log.Printf("github integration disabled: %v", ghErr)
	}

	s := &server{k8s: k8sClient, adminToken: adminToken, gh: ghSvc, audit: auditEmitter}
	s.agentToken = agentToken

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

		r.Post("/github/open-pr", s.handleGitHubOpenPR)
	})

	log.Printf("capability-broker listening on %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, r))
}

type createNetworkGrantRequest struct {
	Namespace string `json:"namespace"`

	// Optional. If empty, server will generate a name.
	Name string `json:"name,omitempty"`

	PodSelector map[string]string `json:"podSelector"`

	PolicyMode workspacesv1alpha1.NetworkGrantPolicyMode `json:"policyMode,omitempty"`
	Protocol   workspacesv1alpha1.NetworkGrantProtocol   `json:"protocol,omitempty"`
	Purpose    string                                    `json:"purpose"`

	Egress []workspacesv1alpha1.NetworkGrantEgressRule `json:"egress,omitempty"`

	AllowNon443 bool `json:"allowNon443,omitempty"`

	TTLSeconds int32 `json:"ttlSeconds,omitempty"`

	Reason string `json:"reason,omitempty"`

	// Optional GitHub context used to request approvals in the PR workflow.
	GitHub *struct {
		Repo       string `json:"repo"`       // owner/repo
		PullNumber int    `json:"pullNumber"` // PR number
	} `json:"github,omitempty"`
}

func (s *server) handleCreateNetworkGrant(w http.ResponseWriter, r *http.Request) {
	if err := s.requireAgent(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

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
	if strings.TrimSpace(req.Purpose) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "purpose_required"})
		return
	}
	if len(req.Egress) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "egress_required"})
		return
	}
	if req.TTLSeconds == 0 {
		req.TTLSeconds = 1800
	}
	if req.PolicyMode == "" {
		req.PolicyMode = workspacesv1alpha1.NetworkGrantPolicyModeStrictFQDN
	}
	if req.Protocol == "" {
		req.Protocol = workspacesv1alpha1.NetworkGrantProtocolTCP
	}

	ng := &workspacesv1alpha1.NetworkGrant{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: req.Namespace,
		},
		Spec: workspacesv1alpha1.NetworkGrantSpec{
			PodSelector: metav1.LabelSelector{MatchLabels: req.PodSelector},
			PolicyMode:  req.PolicyMode,
			Protocol:    req.Protocol,
			Purpose:     strings.TrimSpace(req.Purpose),
			Egress:      req.Egress,
			AllowNon443: req.AllowNon443,
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

	// Optional: if GitHub context is provided, post an approval request to the PR.
	if req.GitHub != nil && s.gh != nil {
		if err := s.gh.commentNetworkGrantRequest(ctx, req.GitHub.Repo, req.GitHub.PullNumber, ng); err != nil {
			log.Printf("github comment for network grant failed: %v", err)
		}
	}

	if s.audit != nil {
		reqID := middleware.GetReqID(ctx)
		s.audit.Emit("networkgrant.request", map[string]any{
			"request_id":    reqID,
			"remote_addr":   r.RemoteAddr,
			"namespace":     ng.Namespace,
			"name":          ng.Name,
			"pod_selector":  req.PodSelector,
			"egress_count":  len(req.Egress),
			"purpose":       strings.TrimSpace(req.Purpose),
			"ttl_seconds":   req.TTLSeconds,
			"allow_non_443": req.AllowNon443,
		})
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

	if s.audit != nil {
		reqID := middleware.GetReqID(ctx)
		fields := map[string]any{
			"request_id":  reqID,
			"remote_addr": r.RemoteAddr,
			"namespace":   ng.Namespace,
			"name":        ng.Name,
			"approved_by": body.ApprovedBy,
		}
		if body.TTLSeconds != nil {
			fields["ttl_seconds"] = *body.TTLSeconds
		}
		if body.Reason != "" {
			fields["reason"] = body.Reason
		}
		s.audit.Emit("networkgrant.approve", fields)
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

func (s *server) requireAgent(r *http.Request) error {
	// Admin token is a superset and may access agent endpoints too.
	if s.adminToken != "" {
		if strings.TrimSpace(r.Header.Get("X-Broker-Admin-Token")) == s.adminToken {
			return nil
		}
	}

	if s.agentToken == "" {
		return errors.New("agent token not configured")
	}
	got := strings.TrimSpace(r.Header.Get("X-Broker-Agent-Token"))
	if got == "" || got != s.agentToken {
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

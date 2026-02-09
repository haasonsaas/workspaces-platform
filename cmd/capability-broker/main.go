package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
	auditpkg "workspaces-platform/internal/audit"
)

type server struct {
	k8s client.Client
	scheme *runtime.Scheme

	audit auditpkg.Emitter

	agentToken   string
	adminToken   string
	webhookToken string

	gh *githubService
}

func main() {
	var (
		listenAddr   = getenv("LISTEN_ADDR", ":8080")
		agentToken   = os.Getenv("BROKER_AGENT_TOKEN")
		adminToken   = os.Getenv("BROKER_ADMIN_TOKEN")
		webhookToken = os.Getenv("BROKER_WEBHOOK_TOKEN")
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

	s := &server{k8s: k8sClient, scheme: scheme, adminToken: adminToken, gh: ghSvc, audit: auditEmitter}
	s.agentToken = agentToken
	s.webhookToken = webhookToken

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
		r.Post("/network-grants/{namespace}/{name}/approve-github", s.handleApproveNetworkGrantGitHub)

		r.Post("/github/open-pr", s.handleGitHubOpenPR)

		r.Post("/agent-jobs", s.handleCreateAgentJob)
	})

	s.startCheckRunReporter()

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
	if strings.TrimSpace(req.Namespace) != "agents" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "namespace_not_allowed"})
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

type createAgentJobRequest struct {
	// Namespace defaults to "agents".
	Namespace string `json:"namespace,omitempty"`

	// Optional. If empty, server will generate a name.
	Name string `json:"name,omitempty"`

	PolicyProfile string `json:"policyProfile,omitempty"`

	TTLSecondsAfterFinished int32 `json:"ttlSecondsAfterFinished,omitempty"`

	GitHub *struct {
		Repo       string `json:"repo"`       // owner/repo
		PullNumber int    `json:"pullNumber"` // PR number
		Actor      string `json:"actor,omitempty"`
		CommentURL string `json:"commentUrl,omitempty"`
	} `json:"github"`
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
	if strings.TrimSpace(ns) != "agents" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "namespace_not_allowed"})
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

func (s *server) handleApproveNetworkGrantGitHub(w http.ResponseWriter, r *http.Request) {
	if err := s.requireWebhook(r); err != nil {
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
	if strings.TrimSpace(ns) != "agents" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "namespace_not_allowed"})
		return
	}

	var body approveNetworkGrantRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}
	if strings.TrimSpace(body.ApprovedBy) == "" {
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
	ng.Spec.ApprovedBy = strings.TrimSpace(body.ApprovedBy)
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
			"approved_by": ng.Spec.ApprovedBy,
			"via":         "github",
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

func (s *server) handleCreateAgentJob(w http.ResponseWriter, r *http.Request) {
	if err := s.requireWebhook(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

	ctx := r.Context()

	var req createAgentJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}

	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = "agents"
	}
	if ns != "agents" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "namespace_not_allowed"})
		return
	}

	if req.GitHub == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "github_context_required"})
		return
	}
	repo := strings.ToLower(strings.TrimSpace(req.GitHub.Repo))
	if repo == "" || !strings.Contains(repo, "/") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "github_repo_required"})
		return
	}
	if req.GitHub.PullNumber <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "github_pullNumber_required"})
		return
	}

	// Enforce repo allowlist using the same GitHub allowlist as broker-only writes.
	if s.gh == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "github_disabled"})
		return
	}
	if !s.gh.repoIsAllowed(repo) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "repo_not_allowed"})
		return
	}

	defaultImage := strings.TrimSpace(getenv("AGENT_DEFAULT_IMAGE", "ghcr.io/workspaces-platform/agent-runner:latest"))
	if defaultImage == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "agent_default_image_not_configured"})
		return
	}

	policyProfile := strings.TrimSpace(req.PolicyProfile)
	if policyProfile == "" {
		policyProfile = strings.TrimSpace(getenv("AGENT_DEFAULT_POLICY_PROFILE", "restricted"))
	}

	ttl := req.TTLSecondsAfterFinished
	if ttl == 0 {
		ttl = 3600
		if raw := strings.TrimSpace(getenv("AGENT_DEFAULT_TTL_SECONDS_AFTER_FINISHED", "")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				ttl = int32(n)
			}
		}
	}

	runtimeClass := strings.TrimSpace(getenv("AGENT_DEFAULT_RUNTIME_CLASS", "kata"))
	if runtimeClass == "" {
		runtimeClass = "kata"
	}

	// Defaults that match docs: schedule onto the dedicated agent pool.
	nodeSelector := map[string]string{"workspaces.platform.dev/pool": "agents"}
	tolerations := []corev1.Toleration{
		{
			Key:      "workspaces.platform.dev/pool",
			Operator: corev1.TolerationOpEqual,
			Value:    "agents",
			Effect:   corev1.TaintEffectNoSchedule,
		},
	}

	script := strings.TrimSpace(os.Getenv("AGENT_DEFAULT_SCRIPT"))
	if script == "" {
		script = `set -euo pipefail
cd "${WORKSPACES_REPO_DIR:-/workspace/repo}"
if [ -x .workspaces/agent.sh ]; then
  exec .workspaces/agent.sh
fi
if [ -f .workspaces/agent.sh ]; then
  bash .workspaces/agent.sh
  exit 0
fi
echo "No .workspaces/agent.sh found; nothing to do."
`
	}

	jobName := strings.ToLower(strings.TrimSpace(req.Name))
	if jobName == "" {
		jobName = fmt.Sprintf("ghpr-%d-%s", req.GitHub.PullNumber, randomSlug())
		if len(jobName) > 63 {
			jobName = jobName[:63]
		}
		jobName = strings.TrimRight(jobName, "-")
	}

	owner, repoName, err := splitRepo(repo)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "github_repo_required"})
		return
	}

	installToken, err := s.gh.getInstallationToken(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "github_token_failed"})
		return
	}

	headSHA, err := s.gh.getPullHeadSHA(ctx, installToken, owner, repoName, req.GitHub.PullNumber)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "github_pull_fetch_failed"})
		return
	}

	var checkRunID int64
	var checkRunURL string
	if s.gh.enableCheckRuns {
		id, url, crErr := s.gh.createCheckRunInProgress(ctx, installToken, owner, repoName, headSHA, ns, jobName)
		if crErr != nil {
			log.Printf("github create check run failed: %v", crErr)
		} else {
			checkRunID = id
			checkRunURL = url
		}
	}

	repoReadToken, repoReadExpiry, err := s.gh.mintRepoReadToken(ctx, repo)
	if err != nil {
		// If we can't mint a scoped read token, don't start the job.
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "github_repo_read_token_failed"})
		return
	}

	annotations := map[string]string{
		"workspaces.platform.dev/source":             "github",
		"workspaces.platform.dev/github-repo":        repo,
		"workspaces.platform.dev/github-pr-number":   fmt.Sprintf("%d", req.GitHub.PullNumber),
		"workspaces.platform.dev/github-comment-url": strings.TrimSpace(req.GitHub.CommentURL),
		"workspaces.platform.dev/github-actor":       strings.TrimSpace(req.GitHub.Actor),
		"workspaces.platform.dev/github-head-sha":    headSHA,
	}
	if checkRunID > 0 {
		annotations["workspaces.platform.dev/github-check-run-id"] = fmt.Sprintf("%d", checkRunID)
	}
	if strings.TrimSpace(checkRunURL) != "" {
		annotations["workspaces.platform.dev/github-check-run-url"] = strings.TrimSpace(checkRunURL)
	}

	ttlPtr := func() *int32 { v := ttl; return &v }()
	runtimePtr := func() *string { v := runtimeClass; return &v }()

	aj := &workspacesv1alpha1.AgentJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:        jobName,
			Namespace:   ns,
			Annotations: annotations,
		},
		Spec: workspacesv1alpha1.AgentJobSpec{
			Image:                   defaultImage,
			Script:                  script,
			GitHub:                  &workspacesv1alpha1.AgentJobGitHubSpec{Repo: repo, PullNumber: int32(req.GitHub.PullNumber), HeadSHA: headSHA},
			NodeSelector:            nodeSelector,
			Tolerations:             tolerations,
			RuntimeClassName:        runtimePtr,
			TTLSecondsAfterFinished: ttlPtr,
			PolicyProfile:           policyProfile,
		},
	}

	if err := s.k8s.Create(ctx, aj); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "create_failed"})
		return
	}

	// Create a short-lived repo read token secret for the Job checkout initContainer.
	secretName := fmt.Sprintf("agentjob-%s-github", aj.Name)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns}}
	_, sErr := controllerutil.CreateOrUpdate(ctx, s.k8s, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels["workspaces.platform.dev/app"] = "agent"
		secret.Labels["workspaces.platform.dev/agentjob"] = aj.Name
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations["workspaces.platform.dev/expires-at"] = repoReadExpiry.UTC().Format(time.RFC3339)
		secret.Type = corev1.SecretTypeOpaque
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data["token"] = []byte(repoReadToken)
		return controllerutil.SetControllerReference(aj, secret, s.scheme)
	})
	if sErr != nil {
		// Cleanup best-effort so we don't strand jobs without checkout creds.
		_ = s.k8s.Delete(ctx, aj)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "github_token_secret_failed"})
		return
	}

	if s.audit != nil {
		reqID := middleware.GetReqID(ctx)
		s.audit.Emit("agentjob.create", map[string]any{
			"request_id":                 reqID,
			"remote_addr":                r.RemoteAddr,
			"namespace":                  aj.Namespace,
			"name":                       aj.Name,
			"repo":                       repo,
			"pull_number":                req.GitHub.PullNumber,
			"head_sha":                   headSHA,
			"check_run_id":               checkRunID,
			"policyProfile":              policyProfile,
			"ttl_seconds_after_finished": ttl,
		})
	}

	// Best-effort: post status comment back to the PR.
	if err := s.gh.commentAgentJobCreated(ctx, repo, req.GitHub.PullNumber, aj.Namespace, aj.Name, policyProfile, checkRunURL); err != nil {
		log.Printf("github comment for agent job failed: %v", err)
	}

	writeJSON(w, http.StatusCreated, aj)
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

func (s *server) requireWebhook(r *http.Request) error {
	// Admin token is a superset.
	if s.adminToken != "" {
		if strings.TrimSpace(r.Header.Get("X-Broker-Admin-Token")) == s.adminToken {
			return nil
		}
	}

	if s.webhookToken == "" {
		return errors.New("webhook token not configured")
	}
	got := strings.TrimSpace(r.Header.Get("X-Broker-Webhook-Token"))
	if got == "" || got != s.webhookToken {
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

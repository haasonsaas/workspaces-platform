package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type server struct {
	webhookSecret []byte

	repoAllowlist     map[string]struct{}
	approverAllowlist map[string]struct{}
	launcherAllowlist map[string]struct{}

	brokerBaseURL string
	brokerHeader  string
	brokerToken   string

	httpClient *http.Client
}

func main() {
	listenAddr := getenv("LISTEN_ADDR", ":8080")

	secret := os.Getenv("GITHUB_WEBHOOK_SECRET")
	if secret == "" {
		log.Fatalf("GITHUB_WEBHOOK_SECRET is required")
	}

	repos := parseCSVSet(os.Getenv("GITHUB_REPO_ALLOWLIST"))
	if len(repos) == 0 {
		log.Fatalf("GITHUB_REPO_ALLOWLIST is empty (secure default: deny all)")
	}

	approvers := parseCSVSet(os.Getenv("GITHUB_APPROVER_ALLOWLIST"))
	if len(approvers) == 0 {
		log.Fatalf("GITHUB_APPROVER_ALLOWLIST is empty (secure default: deny all)")
	}

	launchers := parseCSVSet(os.Getenv("GITHUB_LAUNCHER_ALLOWLIST"))
	if len(launchers) == 0 {
		// Reasonable default: only approvers can trigger agent runs.
		launchers = approvers
	}

	brokerBase := strings.TrimRight(getenv("BROKER_BASE_URL", ""), "/")
	if brokerBase == "" {
		log.Fatalf("BROKER_BASE_URL is required (e.g. http://capability-broker.workspaces-system.svc.cluster.local:8080)")
	}

	brokerHeader := ""
	brokerToken := ""
	if v := strings.TrimSpace(os.Getenv("BROKER_WEBHOOK_TOKEN")); v != "" {
		brokerHeader = "X-Broker-Webhook-Token"
		brokerToken = v
	} else if v := strings.TrimSpace(os.Getenv("BROKER_ADMIN_TOKEN")); v != "" {
		// Backwards-compatible fallback; not recommended.
		brokerHeader = "X-Broker-Admin-Token"
		brokerToken = v
		log.Printf("warning: using BROKER_ADMIN_TOKEN; prefer BROKER_WEBHOOK_TOKEN for least privilege")
	} else {
		log.Fatalf("BROKER_WEBHOOK_TOKEN (preferred) or BROKER_ADMIN_TOKEN is required")
	}

	s := &server{
		webhookSecret:     []byte(secret),
		repoAllowlist:     repos,
		approverAllowlist: approvers,
		launcherAllowlist: launchers,
		brokerBaseURL:     brokerBase,
		brokerHeader:      brokerHeader,
		brokerToken:       brokerToken,
		httpClient:        &http.Client{Timeout: 15 * time.Second},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/github/webhook", s.handleGitHubWebhook)

	log.Printf("github-webhook listening on %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}

func (s *server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	eventType := strings.TrimSpace(r.Header.Get("X-GitHub-Event"))
	if eventType == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20)) // 2 MiB
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if err := s.verifySignature(r.Header.Get("X-Hub-Signature-256"), body); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch eventType {
	case "issue_comment":
		s.handleIssueComment(w, r, body)
	default:
		// Ignore other event types.
		w.WriteHeader(http.StatusAccepted)
	}
}

type issueCommentEvent struct {
	Action string `json:"action"`

	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`

	Issue struct {
		Number      int             `json:"number"`
		PullRequest json.RawMessage `json:"pull_request,omitempty"`
	} `json:"issue"`

	Comment struct {
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
}

func (s *server) handleIssueComment(w http.ResponseWriter, r *http.Request, body []byte) {
	var ev issueCommentEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if ev.Action != "created" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Only PR comments (issue_comment fires for issues and PRs; PRs include pull_request).
	if len(ev.Issue.PullRequest) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	repo := strings.ToLower(strings.TrimSpace(ev.Repository.FullName))
	if repo == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if _, ok := s.repoAllowlist[repo]; !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	actor := strings.ToLower(strings.TrimSpace(ev.Comment.User.Login))
	if actor == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	handled := false

	if cmd, ok := parseNetgrantApproveCommand(ev.Comment.Body); ok {
		if _, ok := s.approverAllowlist[actor]; ok {
			reason := fmt.Sprintf("github:%s#%d %s", repo, ev.Issue.Number, strings.TrimSpace(ev.Comment.HTMLURL))
			if err := s.approveNetworkGrant(r.Context(), cmd, actor, reason); err != nil {
				log.Printf("approve network grant failed: %v", err)
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			handled = true
		}
	}

	if cmd, ok := parseAgentRunCommand(ev.Comment.Body); ok {
		if _, ok := s.launcherAllowlist[actor]; ok {
			if err := s.createAgentJob(r.Context(), repo, ev.Issue.Number, actor, strings.TrimSpace(ev.Comment.HTMLURL), cmd); err != nil {
				log.Printf("create agent job failed: %v", err)
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			handled = true
		}
	}

	if handled {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

type netgrantApproveCommand struct {
	Namespace  string
	Name       string
	TTLSeconds *int32
}

func parseNetgrantApproveCommand(body string) (netgrantApproveCommand, bool) {
	line, ok := findCommandLine(body, "/netgrant")
	if !ok {
		return netgrantApproveCommand{}, false
	}

	parts := strings.Fields(line)
	if len(parts) < 3 {
		return netgrantApproveCommand{}, false
	}
	if parts[0] != "/netgrant" || parts[1] != "approve" {
		return netgrantApproveCommand{}, false
	}

	target := strings.TrimSpace(parts[2])
	if target == "" {
		return netgrantApproveCommand{}, false
	}

	ns := "agents"
	name := target
	if strings.Contains(target, "/") {
		p := strings.Split(target, "/")
		if len(p) != 2 || p[0] == "" || p[1] == "" {
			return netgrantApproveCommand{}, false
		}
		ns = p[0]
		name = p[1]
	}

	var ttl *int32
	for _, p := range parts[3:] {
		if strings.HasPrefix(p, "ttl=") {
			raw := strings.TrimPrefix(p, "ttl=")
			n, err := strconv.ParseInt(raw, 10, 32)
			if err != nil || n <= 0 || n > 24*60*60 {
				return netgrantApproveCommand{}, false
			}
			v := int32(n)
			ttl = &v
		}
	}

	return netgrantApproveCommand{Namespace: ns, Name: name, TTLSeconds: ttl}, true
}

func (s *server) approveNetworkGrant(ctx context.Context, cmd netgrantApproveCommand, approvedBy, reason string) error {
	path := "approve"
	if s.brokerHeader == "X-Broker-Webhook-Token" {
		path = "approve-github"
	}
	u := fmt.Sprintf("%s/v1/network-grants/%s/%s/%s", s.brokerBaseURL, cmd.Namespace, cmd.Name, path)

	payload := map[string]any{
		"approvedBy": approvedBy,
		"reason":     reason,
	}
	if cmd.TTLSeconds != nil {
		payload["ttlSeconds"] = *cmd.TTLSeconds
	}
	b, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(s.brokerHeader, s.brokerToken)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("broker approve: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

type agentRunCommand struct {
	PolicyProfile string
	TTLSeconds    *int32
}

func parseAgentRunCommand(body string) (agentRunCommand, bool) {
	line, ok := findCommandLine(body, "/agent")
	if !ok {
		return agentRunCommand{}, false
	}

	parts := strings.Fields(line)
	if len(parts) < 2 {
		return agentRunCommand{}, false
	}
	if parts[0] != "/agent" || parts[1] != "run" {
		return agentRunCommand{}, false
	}

	var out agentRunCommand
	for _, p := range parts[2:] {
		if strings.HasPrefix(p, "profile=") || strings.HasPrefix(p, "policyProfile=") {
			out.PolicyProfile = strings.TrimSpace(strings.SplitN(p, "=", 2)[1])
			continue
		}
		if strings.HasPrefix(p, "ttl=") {
			raw := strings.TrimSpace(strings.TrimPrefix(p, "ttl="))
			n, err := strconv.ParseInt(raw, 10, 32)
			if err != nil || n <= 0 || n > 24*60*60 {
				return agentRunCommand{}, false
			}
			v := int32(n)
			out.TTLSeconds = &v
			continue
		}
	}

	return out, true
}

func (s *server) createAgentJob(ctx context.Context, repo string, prNumber int, actor, commentURL string, cmd agentRunCommand) error {
	u := fmt.Sprintf("%s/v1/agent-jobs", s.brokerBaseURL)

	payload := map[string]any{
		"github": map[string]any{
			"repo":       repo,
			"pullNumber": prNumber,
			"actor":      actor,
			"commentUrl": commentURL,
		},
	}
	if strings.TrimSpace(cmd.PolicyProfile) != "" {
		payload["policyProfile"] = strings.TrimSpace(cmd.PolicyProfile)
	}
	if cmd.TTLSeconds != nil {
		payload["ttlSecondsAfterFinished"] = *cmd.TTLSeconds
	}
	b, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(s.brokerHeader, s.brokerToken)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("broker create agent job: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func (s *server) verifySignature(sigHeader string, body []byte) error {
	sigHeader = strings.TrimSpace(sigHeader)
	if !strings.HasPrefix(sigHeader, "sha256=") {
		return errors.New("missing sha256 signature")
	}
	wantHex := strings.TrimPrefix(sigHeader, "sha256=")
	want, err := hex.DecodeString(wantHex)
	if err != nil || len(want) != sha256.Size {
		return errors.New("invalid signature encoding")
	}

	mac := hmac.New(sha256.New, s.webhookSecret)
	_, _ = mac.Write(body)
	got := mac.Sum(nil)

	if !hmac.Equal(got, want) {
		return errors.New("signature mismatch")
	}
	return nil
}

func parseCSVSet(csv string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, part := range strings.Split(csv, ",") {
		s := strings.ToLower(strings.TrimSpace(part))
		if s == "" {
			continue
		}
		out[s] = struct{}{}
	}
	return out
}

func findCommandLine(body string, prefix string) (string, bool) {
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		line = strings.Trim(line, "`")
		if strings.HasPrefix(line, prefix) {
			return line, true
		}
	}
	return "", false
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

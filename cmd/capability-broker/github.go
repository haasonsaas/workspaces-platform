package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/golang-jwt/jwt/v5"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
	auditpkg "workspaces-platform/internal/audit"
)

type githubService struct {
	apiBase string
	gitBase string

	appID          int64
	installationID int64
	privateKey     *rsa.PrivateKey

	repoAllowlist map[string]struct{}

	defaultBase  string
	allowedBases map[string]struct{}

	branchPrefix string

	authorName  string
	authorEmail string

	// Patch safety limits.
	maxFilesChanged    int
	allowBinaryPatches bool

	// Sensitive paths are denied by default unless the repo is explicitly allowlisted.
	sensitivePathPrefixDenylist []string
	sensitivePathAllowlistRepos map[string]struct{}

	enableCheckRuns bool
	checkRunName    string

	audit auditpkg.Emitter

	httpClient *http.Client

	mu           sync.Mutex
	cachedToken  string
	cachedExpiry time.Time
}

func newGitHubServiceFromEnv(audit auditpkg.Emitter) (*githubService, error) {
	appID, ok, err := envInt64("GITHUB_APP_ID")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("GITHUB_APP_ID not set")
	}

	installationID, ok, err := envInt64("GITHUB_APP_INSTALLATION_ID")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("GITHUB_APP_INSTALLATION_ID not set")
	}

	keyBytes, err := loadKeyMaterialFromEnv()
	if err != nil {
		return nil, err
	}
	key, err := parseRSAPrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub app private key: %w", err)
	}

	repoAllowlist := parseCSVSet(os.Getenv("GITHUB_REPO_ALLOWLIST"))
	if len(repoAllowlist) == 0 {
		return nil, errors.New("GITHUB_REPO_ALLOWLIST is empty (secure default: deny all)")
	}

	defaultBase := getenv("GITHUB_DEFAULT_BASE_BRANCH", "main")
	allowedBases := parseCSVSet(getenv("GITHUB_BASE_BRANCH_ALLOWLIST", defaultBase))
	if len(allowedBases) == 0 {
		return nil, errors.New("GITHUB_BASE_BRANCH_ALLOWLIST is empty")
	}

	branchPrefix := getenv("GITHUB_BRANCH_PREFIX", "agent/")

	apiBase := strings.TrimRight(getenv("GITHUB_API_URL", "https://api.github.com"), "/")
	gitBase := strings.TrimRight(getenv("GITHUB_GIT_URL", "https://github.com"), "/")

	authorName := getenv("GITHUB_GIT_AUTHOR_NAME", "workspaces-broker")
	authorEmail := getenv("GITHUB_GIT_AUTHOR_EMAIL", "workspaces-broker@localhost")

	maxFilesChanged := 100
	if raw := strings.TrimSpace(getenv("GITHUB_MAX_FILES_CHANGED", "100")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, errors.New("GITHUB_MAX_FILES_CHANGED must be a positive integer")
		}
		maxFilesChanged = n
	}

	sensitiveDeny := parseCSVList(getenv("GITHUB_SENSITIVE_PATH_PREFIX_DENYLIST", ".github/workflows/,infra/,terraform/,deploy/"))
	sensitiveAllowRepos := parseCSVSet(os.Getenv("GITHUB_SENSITIVE_PATH_ALLOWLIST_REPOS"))

	allowBinary := strings.EqualFold(strings.TrimSpace(getenv("GITHUB_ALLOW_BINARY_PATCHES", "false")), "true")

	enableCheckRuns := strings.EqualFold(strings.TrimSpace(getenv("GITHUB_ENABLE_CHECK_RUNS", "false")), "true")
	checkRunName := strings.TrimSpace(getenv("GITHUB_CHECK_RUN_NAME", "workspaces-agent"))
	if checkRunName == "" {
		checkRunName = "workspaces-agent"
	}

	return &githubService{
		apiBase:                     apiBase,
		gitBase:                     gitBase,
		appID:                       appID,
		installationID:              installationID,
		privateKey:                  key,
		repoAllowlist:               repoAllowlist,
		defaultBase:                 defaultBase,
		allowedBases:                allowedBases,
		branchPrefix:                branchPrefix,
		authorName:                  authorName,
		authorEmail:                 authorEmail,
		maxFilesChanged:             maxFilesChanged,
		allowBinaryPatches:          allowBinary,
		sensitivePathPrefixDenylist: sensitiveDeny,
		sensitivePathAllowlistRepos: sensitiveAllowRepos,
		enableCheckRuns:             enableCheckRuns,
		checkRunName:                checkRunName,
		audit:                       audit,
		httpClient:                  &http.Client{Timeout: 30 * time.Second},
		cachedToken:                 "",
		cachedExpiry:                time.Time{},
	}, nil
}

func loadKeyMaterialFromEnv() ([]byte, error) {
	if pemText := os.Getenv("GITHUB_APP_PRIVATE_KEY_PEM"); pemText != "" {
		return []byte(pemText), nil
	}
	if path := os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read GITHUB_APP_PRIVATE_KEY_FILE: %w", err)
		}
		return b, nil
	}
	return nil, errors.New("missing GitHub key material: set GITHUB_APP_PRIVATE_KEY_PEM or GITHUB_APP_PRIVATE_KEY_FILE")
}

func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("invalid PEM")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rsaKey, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("expected RSA key, got %T", k)
		}
		return rsaKey, nil
	}
}

func envInt64(name string) (v int64, ok bool, _ error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%s must be an integer", name)
	}
	return n, true, nil
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

func parseCSVList(csv string) []string {
	out := []string{}
	for _, part := range strings.Split(csv, ",") {
		s := strings.TrimSpace(part)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (g *githubService) repoIsAllowed(repo string) bool {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if repo == "" {
		return false
	}
	_, ok := g.repoAllowlist[repo]
	return ok
}

type githubOpenPRRequest struct {
	Repo string `json:"repo"` // owner/repo

	Base string `json:"base,omitempty"` // default main

	Title string `json:"title"`
	Body  string `json:"body,omitempty"`

	// Patch is a unified diff. It is applied onto Base and committed.
	Patch string `json:"patch"`

	CommitMessage string `json:"commitMessage,omitempty"`
	Branch        string `json:"branch,omitempty"` // must start with prefix if provided
	Draft         bool   `json:"draft,omitempty"`
}

type githubOpenPRResponse struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Base   string `json:"base"`
	Number int    `json:"number"`
	URL    string `json:"url"`

	PatchSHA256 string `json:"patchSha256"`
}

func (s *server) handleGitHubOpenPR(w http.ResponseWriter, r *http.Request) {
	if s.gh == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "github_disabled",
			"hint":  "Configure GITHUB_APP_* env vars and GITHUB_REPO_ALLOWLIST to enable broker-only PR creation.",
		})
		return
	}

	if err := s.requireAgent(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

	// Cap body size: patches can be large, but we don't want unbounded memory usage.
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20) // 5 MiB

	var req githubOpenPRRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}

	resp, err := s.gh.openPRFromPatch(r.Context(), r, req)
	if err != nil {
		var herr httpError
		if errors.As(err, &herr) {
			writeJSON(w, herr.Status, map[string]any{"error": herr.Code})
			return
		}
		log.Printf("github open-pr failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal_error"})
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

type httpError struct {
	Status int
	Code   string
	Err    error
}

func (e httpError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return e.Code
}

func (e httpError) Unwrap() error { return e.Err }

func invalidGitRef(ref string) bool {
	if strings.TrimSpace(ref) != ref {
		return true
	}
	if strings.ContainsAny(ref, " \t\r\n") {
		return true
	}
	if strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") {
		return true
	}
	if strings.Contains(ref, "..") {
		return true
	}
	if strings.Contains(ref, "@{") {
		return true
	}
	// Disallow obvious ref/path injection characters.
	if strings.ContainsAny(ref, "~^:?*[]\\") {
		return true
	}
	return false
}

func (g *githubService) openPRFromPatch(ctx context.Context, r *http.Request, req githubOpenPRRequest) (*githubOpenPRResponse, error) {
	repo := strings.ToLower(strings.TrimSpace(req.Repo))
	if repo == "" || !strings.Contains(repo, "/") {
		return nil, httpError{Status: http.StatusBadRequest, Code: "repo_required"}
	}
	if _, ok := g.repoAllowlist[repo]; !ok {
		return nil, httpError{Status: http.StatusForbidden, Code: "repo_not_allowed"}
	}

	base := strings.TrimSpace(req.Base)
	if base == "" {
		base = g.defaultBase
	}
	if _, ok := g.allowedBases[strings.ToLower(base)]; !ok {
		return nil, httpError{Status: http.StatusBadRequest, Code: "base_not_allowed"}
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		return nil, httpError{Status: http.StatusBadRequest, Code: "title_required"}
	}

	patch := req.Patch
	if strings.TrimSpace(patch) == "" {
		return nil, httpError{Status: http.StatusBadRequest, Code: "patch_required"}
	}

	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = g.branchPrefix + randomSlug()
	}
	if !strings.HasPrefix(branch, g.branchPrefix) {
		return nil, httpError{Status: http.StatusBadRequest, Code: "branch_prefix_required"}
	}
	if invalidGitRef(branch) {
		return nil, httpError{Status: http.StatusBadRequest, Code: "invalid_branch"}
	}

	commitMsg := strings.TrimSpace(req.CommitMessage)
	if commitMsg == "" {
		commitMsg = "agent: " + title
	}

	patchHash := sha256.Sum256([]byte(patch))
	patchHashHex := hex.EncodeToString(patchHash[:])

	token, err := g.getInstallationToken(ctx)
	if err != nil {
		return nil, err
	}

	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, httpError{Status: http.StatusBadRequest, Code: "invalid_repo", Err: err}
	}

	tmpRoot := getenv("BROKER_TMP_DIR", "")
	tmpDir, err := os.MkdirTemp(tmpRoot, "wsp-broker-gh-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	repoDir := filepath.Join(tmpDir, "repo")
	patchPath := filepath.Join(tmpDir, "patch.diff")
	if err := os.WriteFile(patchPath, []byte(patch), 0600); err != nil {
		return nil, err
	}

	askpassPath := filepath.Join(tmpDir, "askpass.sh")
	if err := writeAskpass(askpassPath); err != nil {
		return nil, err
	}

	baseURL := fmt.Sprintf("%s/%s/%s.git", g.gitBase, owner, name)
	env := brokerGitEnv(tmpDir, askpassPath, token)

	// Clone base branch.
	if err := runGit(ctx, tmpDir, env, "clone", "--depth", "1", "--branch", base, baseURL, repoDir); err != nil {
		return nil, err
	}

	// Configure identity.
	if err := runGit(ctx, repoDir, env, "config", "user.name", g.authorName); err != nil {
		return nil, err
	}
	if err := runGit(ctx, repoDir, env, "config", "user.email", g.authorEmail); err != nil {
		return nil, err
	}
	_ = runGit(ctx, repoDir, env, "config", "commit.gpgsign", "false")

	// Create branch, apply patch, commit.
	if err := runGit(ctx, repoDir, env, "checkout", "-b", branch); err != nil {
		return nil, err
	}
	// Apply patch into the index. Fail closed for binary patches unless explicitly enabled.
	applyArgs := []string{"apply", "--index", "--whitespace=nowarn"}
	if g.allowBinaryPatches {
		applyArgs = append(applyArgs, "--binary")
	}
	applyArgs = append(applyArgs, patchPath)
	if err := runGit(ctx, repoDir, env, applyArgs...); err != nil {
		return nil, httpError{Status: http.StatusBadRequest, Code: "patch_apply_failed", Err: err}
	}

	changedFiles, err := g.changedFiles(ctx, repoDir, env)
	if err != nil {
		return nil, err
	}
	if len(changedFiles) == 0 {
		return nil, httpError{Status: http.StatusBadRequest, Code: "patch_noop"}
	}
	if g.maxFilesChanged > 0 && len(changedFiles) > g.maxFilesChanged {
		return nil, httpError{Status: http.StatusBadRequest, Code: "patch_too_many_files"}
	}
	if err := g.enforceSensitivePathDenylist(repo, changedFiles); err != nil {
		return nil, err
	}
	if !g.allowBinaryPatches {
		hasBinary, err := gitIndexHasBinaryDiff(ctx, repoDir, env)
		if err != nil {
			return nil, err
		}
		if hasBinary {
			return nil, httpError{Status: http.StatusBadRequest, Code: "patch_binary_not_allowed"}
		}
	}
	if err := gitDiffCheck(ctx, repoDir, env); err != nil {
		return nil, err
	}

	if err := runGit(ctx, repoDir, env, "commit", "-m", commitMsg, "--no-gpg-sign"); err != nil {
		return nil, httpError{Status: http.StatusBadRequest, Code: "commit_failed", Err: err}
	}

	if err := runGit(ctx, repoDir, env, "push", "origin", branch); err != nil {
		return nil, err
	}

	prNum, prURL, err := g.createPR(ctx, token, owner, name, title, req.Body, branch, base, req.Draft)
	if err != nil {
		return nil, err
	}

	reqID := middleware.GetReqID(ctx)
	touchedFiles, truncated := truncateStrings(changedFiles, 50)
	if g.audit != nil {
		g.audit.Emit("github.open_pr", map[string]any{
			"request_id":              reqID,
			"remote_addr":             r.RemoteAddr,
			"repo":                    repo,
			"base":                    base,
			"branch":                  branch,
			"pr_number":               prNum,
			"pr_url":                  prURL,
			"patch_sha256":            patchHashHex,
			"commit_message":          commitMsg,
			"touched_files_count":     len(changedFiles),
			"touched_files_truncated": truncated,
			"touched_files":           touchedFiles,
		})
	}

	return &githubOpenPRResponse{
		Repo:        repo,
		Branch:      branch,
		Base:        base,
		Number:      prNum,
		URL:         prURL,
		PatchSHA256: patchHashHex,
	}, nil
}

func (g *githubService) commentNetworkGrantRequest(ctx context.Context, repo string, prNumber int, ng *workspacesv1alpha1.NetworkGrant) error {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if repo == "" || !strings.Contains(repo, "/") {
		return fmt.Errorf("invalid repo %q", repo)
	}
	if _, ok := g.repoAllowlist[repo]; !ok {
		return fmt.Errorf("repo not allowlisted for github comments")
	}
	if prNumber <= 0 {
		return fmt.Errorf("invalid prNumber %d", prNumber)
	}

	token, err := g.getInstallationToken(ctx)
	if err != nil {
		return err
	}
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}

	body := formatNetworkGrantApprovalComment(ng)
	commentURL, err := g.createIssueComment(ctx, token, owner, name, prNumber, body)
	if err != nil {
		return err
	}

	if g.audit != nil {
		g.audit.Emit("github.comment_networkgrant_request", map[string]any{
			"repo":        repo,
			"pr_number":   prNumber,
			"comment_url": commentURL,
			"grant_ns":    ng.Namespace,
			"grant_name":  ng.Name,
		})
	}

	return nil
}

func (g *githubService) commentAgentJobCreated(ctx context.Context, repo string, prNumber int, jobNS, jobName, policyProfile, checkRunURL string) error {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if repo == "" || !strings.Contains(repo, "/") {
		return fmt.Errorf("invalid repo %q", repo)
	}
	if _, ok := g.repoAllowlist[repo]; !ok {
		return fmt.Errorf("repo not allowlisted for github comments")
	}
	if prNumber <= 0 {
		return fmt.Errorf("invalid prNumber %d", prNumber)
	}

	token, err := g.getInstallationToken(ctx)
	if err != nil {
		return err
	}
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}

	body := formatAgentJobCreatedComment(jobNS, jobName, policyProfile, checkRunURL)
	commentURL, err := g.createIssueComment(ctx, token, owner, name, prNumber, body)
	if err != nil {
		return err
	}

	if g.audit != nil {
		g.audit.Emit("github.comment_agentjob_created", map[string]any{
			"repo":          repo,
			"pr_number":     prNumber,
			"comment_url":   commentURL,
			"check_run_url": checkRunURL,
			"job_ns":        jobNS,
			"job_name":      jobName,
		})
	}

	return nil
}

func formatAgentJobCreatedComment(jobNS, jobName, policyProfile, checkRunURL string) string {
	var b strings.Builder
	b.WriteString("Agent job started.\n\n")
	b.WriteString("- AgentJob: `" + strings.TrimSpace(jobNS) + "/" + strings.TrimSpace(jobName) + "`\n")
	if strings.TrimSpace(policyProfile) != "" {
		b.WriteString("- PolicyProfile: `" + strings.TrimSpace(policyProfile) + "`\n")
	}
	if strings.TrimSpace(checkRunURL) != "" {
		b.WriteString("- Check: " + strings.TrimSpace(checkRunURL) + "\n")
	}
	b.WriteString("\nTrack:\n")
	b.WriteString("`kubectl -n " + strings.TrimSpace(jobNS) + " get agentjobs " + strings.TrimSpace(jobName) + " -o yaml`\n")
	return b.String()
}

func formatNetworkGrantApprovalComment(ng *workspacesv1alpha1.NetworkGrant) string {
	// Keep this intentionally plain and unambiguous. This is an approval surface.
	var b strings.Builder
	b.WriteString("Network access request for an agent job.\n\n")
	b.WriteString("Grant:\n")
	b.WriteString("- Namespace: `" + ng.Namespace + "`\n")
	b.WriteString("- Name: `" + ng.Name + "`\n")
	if strings.TrimSpace(ng.Spec.Purpose) != "" {
		b.WriteString("- Purpose: " + strings.TrimSpace(ng.Spec.Purpose) + "\n")
	}
	b.WriteString("\nDestinations:\n")
	for _, r := range ng.Spec.Egress {
		host := strings.TrimSpace(r.Host)
		if host == "" {
			continue
		}
		ports := r.Ports
		if len(ports) == 0 {
			ports = []int32{443}
		}
		b.WriteString("- `" + host + "` ports: `")
		for i, p := range ports {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(fmt.Sprintf("%d", p))
		}
		b.WriteString("`\n")
	}
	b.WriteString("\nApprove with:\n")
	b.WriteString("`/netgrant approve " + ng.Namespace + "/" + ng.Name + "`\n")
	b.WriteString("\nOptional TTL override:\n")
	b.WriteString("`/netgrant approve " + ng.Namespace + "/" + ng.Name + " ttl=1800`\n")
	return b.String()
}

func (g *githubService) changedFiles(ctx context.Context, repoDir string, env []string) ([]string, error) {
	out, err := runGitOutput(ctx, repoDir, env, "diff", "--cached", "--name-only")
	if err != nil {
		return nil, err
	}
	files := splitNonEmptyLines(out)
	for _, f := range files {
		if strings.HasPrefix(f, "/") || strings.Contains(f, "..") {
			return nil, httpError{Status: http.StatusBadRequest, Code: "patch_invalid_path"}
		}
	}
	return files, nil
}

func (g *githubService) enforceSensitivePathDenylist(repo string, changed []string) error {
	if len(g.sensitivePathPrefixDenylist) == 0 {
		return nil
	}
	if _, ok := g.sensitivePathAllowlistRepos[repo]; ok {
		return nil
	}
	for _, f := range changed {
		if matchesAnyPrefix(f, g.sensitivePathPrefixDenylist) {
			return httpError{Status: http.StatusBadRequest, Code: "patch_touches_sensitive_paths"}
		}
	}
	return nil
}

func matchesAnyPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if pathMatchesPrefix(path, p) {
			return true
		}
	}
	return false
}

func pathMatchesPrefix(path, prefix string) bool {
	p := strings.TrimSpace(prefix)
	if p == "" {
		return false
	}
	if strings.HasSuffix(p, "/") {
		dir := strings.TrimSuffix(p, "/")
		return path == dir || strings.HasPrefix(path, p)
	}
	return path == p || strings.HasPrefix(path, p+"/")
}

func gitDiffCheck(ctx context.Context, repoDir string, env []string) error {
	out, err := runGitOutput(ctx, repoDir, env, "diff", "--cached", "--check")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	return httpError{Status: http.StatusBadRequest, Code: "patch_diff_check_failed"}
}

func gitIndexHasBinaryDiff(ctx context.Context, repoDir string, env []string) (bool, error) {
	out, err := runGitOutput(ctx, repoDir, env, "diff", "--cached", "--numstat")
	if err != nil {
		return false, err
	}
	for _, line := range splitNonEmptyLines(out) {
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		if parts[0] == "-" && parts[1] == "-" {
			return true, nil
		}
	}
	return false, nil
}

func splitNonEmptyLines(s string) []string {
	raw := strings.Split(s, "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

func truncateStrings(in []string, n int) (out []string, truncated bool) {
	if n <= 0 || len(in) <= n {
		return in, false
	}
	return in[:n], true
}

func brokerGitEnv(tmpDir, askpassPath, token string) []string {
	home := filepath.Join(tmpDir, "home")
	_ = os.MkdirAll(home, 0700)
	return []string{
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=" + askpassPath,
		"WORKSPACES_GIT_PASSWORD=" + token,
	}
}

func writeAskpass(path string) error {
	// Token is passed via env to avoid writing it to disk or process args.
	// Git may prompt for username/password; handle both.
	script := `#!/bin/sh
case "$1" in
*Username*) echo "x-access-token" ;;
*) echo "$WORKSPACES_GIT_PASSWORD" ;;
esac
`
	return os.WriteFile(path, []byte(script), 0700)
}

func runGit(ctx context.Context, dir string, extraEnv []string, args ...string) error {
	cmd := execCommand(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)

	// Capture output for errors; never print tokens (we keep tokens out of args).
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 8<<10 {
			msg = msg[:8<<10] + "...(truncated)"
		}
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return nil
}

func runGitOutput(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	cmd := execCommand(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 8<<10 {
			msg = msg[:8<<10] + "...(truncated)"
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return string(out), nil
}

func splitRepo(repo string) (owner string, name string, _ error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected owner/repo, got %q", repo)
	}
	return parts[0], parts[1], nil
}

func randomSlug() string {
	// time prefix + random suffix to reduce collisions.
	now := time.Now().UTC().Format("20060102-150405")
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%x", now, b[:])
}

func (g *githubService) getInstallationToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	if g.cachedToken != "" && time.Now().Before(g.cachedExpiry.Add(-2*time.Minute)) {
		t := g.cachedToken
		g.mu.Unlock()
		return t, nil
	}
	g.mu.Unlock()

	jwtToken, err := g.appJWT()
	if err != nil {
		return "", err
	}

	u := fmt.Sprintf("%s/app/installations/%d/access_tokens", g.apiBase, g.installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(nil))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github token endpoint: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	if parsed.Token == "" || parsed.ExpiresAt == "" {
		return "", errors.New("github token endpoint returned empty token/expiry")
	}

	exp, err := time.Parse(time.RFC3339, parsed.ExpiresAt)
	if err != nil {
		return "", err
	}

	g.mu.Lock()
	g.cachedToken = parsed.Token
	g.cachedExpiry = exp
	g.mu.Unlock()

	return parsed.Token, nil
}

func (g *githubService) mintRepoReadToken(ctx context.Context, repo string) (token string, expiresAt time.Time, _ error) {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if repo == "" || !strings.Contains(repo, "/") {
		return "", time.Time{}, fmt.Errorf("invalid repo %q", repo)
	}
	if _, ok := g.repoAllowlist[repo]; !ok {
		return "", time.Time{}, fmt.Errorf("repo not allowlisted for mintRepoReadToken")
	}

	_, name, err := splitRepo(repo)
	if err != nil {
		return "", time.Time{}, err
	}

	jwtToken, err := g.appJWT()
	if err != nil {
		return "", time.Time{}, err
	}

	u := fmt.Sprintf("%s/app/installations/%d/access_tokens", g.apiBase, g.installationID)
	payload := map[string]any{
		"repositories": []string{name},
		"permissions": map[string]any{
			"contents": "read",
		},
	}
	b, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", time.Time{}, fmt.Errorf("github token endpoint (scoped): status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", time.Time{}, err
	}
	if parsed.Token == "" || parsed.ExpiresAt == "" {
		return "", time.Time{}, errors.New("github token endpoint returned empty token/expiry")
	}

	exp, err := time.Parse(time.RFC3339, parsed.ExpiresAt)
	if err != nil {
		return "", time.Time{}, err
	}
	return parsed.Token, exp, nil
}

func (g *githubService) appJWT() (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Issuer:    fmt.Sprintf("%d", g.appID),
		IssuedAt:  jwt.NewNumericDate(now.Add(-30 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return tok.SignedString(g.privateKey)
}

func (g *githubService) createPR(ctx context.Context, token, owner, repo, title, body, head, base string, draft bool) (int, string, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/pulls", g.apiBase, owner, repo)
	payload := map[string]any{
		"title": title,
		"head":  head,
		"base":  base,
		"draft": draft,
	}
	if strings.TrimSpace(body) != "" {
		payload["body"] = body
	}
	b, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, "", fmt.Errorf("github create pr: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return 0, "", err
	}
	if parsed.Number == 0 || parsed.HTMLURL == "" {
		return 0, "", errors.New("github create pr returned empty number/url")
	}
	return parsed.Number, parsed.HTMLURL, nil
}

func (g *githubService) createIssueComment(ctx context.Context, token, owner, repo string, issueNumber int, body string) (string, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", g.apiBase, owner, repo, issueNumber)
	payload := map[string]any{"body": body}
	b, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github create comment: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", err
	}
	if parsed.HTMLURL == "" {
		return "", errors.New("github create comment returned empty url")
	}
	return parsed.HTMLURL, nil
}

func (g *githubService) getPullHeadSHA(ctx context.Context, token, owner, repo string, prNumber int) (string, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", g.apiBase, owner, repo, prNumber)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github get pull: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", err
	}
	if strings.TrimSpace(parsed.Head.SHA) == "" {
		return "", errors.New("github get pull returned empty head sha")
	}
	return strings.TrimSpace(parsed.Head.SHA), nil
}

func (g *githubService) createCheckRunInProgress(ctx context.Context, token, owner, repo, headSHA, jobNS, jobName string) (int64, string, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/check-runs", g.apiBase, owner, repo)

	payload := map[string]any{
		"name":        g.checkRunName,
		"head_sha":    headSHA,
		"status":      "in_progress",
		"external_id": fmt.Sprintf("%s/%s", strings.TrimSpace(jobNS), strings.TrimSpace(jobName)),
		"output": map[string]any{
			"title":   "Agent job started",
			"summary": fmt.Sprintf("AgentJob: %s/%s", strings.TrimSpace(jobNS), strings.TrimSpace(jobName)),
		},
	}
	b, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, "", fmt.Errorf("github create check run: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return 0, "", err
	}
	return parsed.ID, strings.TrimSpace(parsed.HTMLURL), nil
}

func (g *githubService) completeCheckRun(ctx context.Context, repo string, checkRunID int64, conclusion, summary string) (string, error) {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if repo == "" || !strings.Contains(repo, "/") {
		return "", fmt.Errorf("invalid repo %q", repo)
	}
	if _, ok := g.repoAllowlist[repo]; !ok {
		return "", fmt.Errorf("repo not allowlisted for check runs")
	}
	if checkRunID <= 0 {
		return "", fmt.Errorf("invalid checkRunID %d", checkRunID)
	}

	token, err := g.getInstallationToken(ctx)
	if err != nil {
		return "", err
	}
	owner, name, err := splitRepo(repo)
	if err != nil {
		return "", err
	}

	u := fmt.Sprintf("%s/repos/%s/%s/check-runs/%d", g.apiBase, owner, name, checkRunID)
	now := time.Now().UTC().Format(time.RFC3339)
	payload := map[string]any{
		"status":       "completed",
		"completed_at": now,
		"conclusion":   conclusion,
		"output": map[string]any{
			"title":   "Agent job " + conclusion,
			"summary": strings.TrimSpace(summary),
		},
	}
	b, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github update check run: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", err
	}
	return strings.TrimSpace(parsed.HTMLURL), nil
}

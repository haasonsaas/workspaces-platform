package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
)

func main() {
	ctrl.SetLogger(ctrl.Log.WithName("wsctl"))

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "desktop":
		cmdDesktop(os.Args[2:])
	case "agent":
		cmdAgent(os.Args[2:])
	case "netgrant":
		cmdNetgrant(os.Args[2:])
	case "github":
		cmdGitHub(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `wsctl: minimal CLI for workspaces-platform

Usage:
  wsctl desktop <create|get|list|delete> ...
  wsctl agent   <create|run-pr|get|list|delete> ...
  wsctl netgrant <request|approve> ...
  wsctl github  <open-pr> ...

Env:
  WORKSPACES_BROKER_URL   Base URL for capability-broker (e.g. http://capability-broker.workspaces-system.svc.cluster.local:8080)
  BROKER_AGENT_TOKEN      Token for agent endpoints (X-Broker-Agent-Token)
  BROKER_ADMIN_TOKEN      Token for admin endpoints (X-Broker-Admin-Token)
  BROKER_WEBHOOK_TOKEN    Token for webhook endpoints (X-Broker-Webhook-Token)

Kubernetes:
  Uses the default kubeconfig resolution (KUBECONFIG or ~/.kube/config).
`)
}

func cmdDesktop(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("desktop create", flag.ExitOnError)
		var (
			name       = fs.String("name", "", "Desktop name")
			namespace  = fs.String("namespace", "desktops", "Namespace")
			user       = fs.String("user", "", "Linux username in the desktop")
			image      = fs.String("image", "", "Desktop image (optional)")
			sshKeyFile = fs.String("ssh-key-file", "", "Path to SSH public key file")
			homeSize   = fs.String("home-size", "50Gi", "Home PVC size (e.g. 50Gi)")
			storageCls = fs.String("storage-class", "", "Home PVC storage class (optional)")
		)
		fs.Parse(args[1:])

		if strings.TrimSpace(*name) == "" || strings.TrimSpace(*user) == "" || strings.TrimSpace(*sshKeyFile) == "" {
			fs.Usage()
			os.Exit(2)
		}

		keyBytes, err := os.ReadFile(*sshKeyFile)
		dieIf(err)
		pubKey := strings.TrimSpace(string(keyBytes))
		if pubKey == "" {
			die("empty ssh public key")
		}

		k, err := newK8sClient()
		dieIf(err)

		var scn *string
		if strings.TrimSpace(*storageCls) != "" {
			v := strings.TrimSpace(*storageCls)
			scn = &v
		}

		desk := &workspacesv1alpha1.Desktop{
			ObjectMeta: metav1.ObjectMeta{
				Name:      strings.TrimSpace(*name),
				Namespace: strings.TrimSpace(*namespace),
			},
			Spec: workspacesv1alpha1.DesktopSpec{
				User:  strings.TrimSpace(*user),
				Image: strings.TrimSpace(*image),
				SSH:   workspacesv1alpha1.DesktopSSHSpec{AuthorizedKeys: []string{pubKey}},
				Home: workspacesv1alpha1.DesktopHomeSpec{
					StorageClassName: scn,
					Size:             strings.TrimSpace(*homeSize),
				},
				NodeSelector: map[string]string{"workspaces.platform.dev/pool": "desktops"},
				Tolerations: []corev1.Toleration{{
					Key:      "workspaces.platform.dev/pool",
					Operator: corev1.TolerationOpEqual,
					Value:    "desktops",
					Effect:   corev1.TaintEffectNoSchedule,
				}},
			},
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dieIf(k.Create(ctx, desk))
		fmt.Printf("created Desktop %s/%s\n", desk.Namespace, desk.Name)

	case "get":
		fs := flag.NewFlagSet("desktop get", flag.ExitOnError)
		name := fs.String("name", "", "Desktop name")
		namespace := fs.String("namespace", "desktops", "Namespace")
		fs.Parse(args[1:])
		if strings.TrimSpace(*name) == "" {
			fs.Usage()
			os.Exit(2)
		}
		k, err := newK8sClient()
		dieIf(err)
		var desk workspacesv1alpha1.Desktop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dieIf(k.Get(ctx, client.ObjectKey{Namespace: *namespace, Name: *name}, &desk))
		out, _ := json.MarshalIndent(desk, "", "  ")
		fmt.Println(string(out))

	case "list":
		fs := flag.NewFlagSet("desktop list", flag.ExitOnError)
		namespace := fs.String("namespace", "desktops", "Namespace")
		fs.Parse(args[1:])
		k, err := newK8sClient()
		dieIf(err)
		var list workspacesv1alpha1.DesktopList
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dieIf(k.List(ctx, &list, client.InNamespace(*namespace)))
		for _, d := range list.Items {
			fmt.Printf("%s/%s phase=%s service=%s\n", d.Namespace, d.Name, d.Status.Phase, d.Status.ServiceName)
		}

	case "delete":
		fs := flag.NewFlagSet("desktop delete", flag.ExitOnError)
		name := fs.String("name", "", "Desktop name")
		namespace := fs.String("namespace", "desktops", "Namespace")
		fs.Parse(args[1:])
		if strings.TrimSpace(*name) == "" {
			fs.Usage()
			os.Exit(2)
		}
		k, err := newK8sClient()
		dieIf(err)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dieIf(k.Delete(ctx, &workspacesv1alpha1.Desktop{ObjectMeta: metav1.ObjectMeta{Name: *name, Namespace: *namespace}}))
		fmt.Printf("deleted Desktop %s/%s\n", *namespace, *name)

	default:
		usage()
		os.Exit(2)
	}
}

func cmdAgent(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("agent create", flag.ExitOnError)
		var (
			name      = fs.String("name", "", "AgentJob name")
			namespace = fs.String("namespace", "agents", "Namespace")
			image     = fs.String("image", "ghcr.io/workspaces-platform/agent-runner:latest", "Agent image")
			script    = fs.String("script", "", "Script to run (agent runner mode; recommended)")
			workdir   = fs.String("workdir", "", "Working directory for --script (optional)")
			shell     = fs.String("shell", "echo hello from agent", "Shell script to run (direct mode; bash -lc)")
			profile   = fs.String("policy-profile", "restricted", "Policy profile")
			ttl       = fs.Int("ttl", 3600, "TTL seconds after finished")
		)
		fs.Parse(args[1:])
		if strings.TrimSpace(*name) == "" {
			fs.Usage()
			os.Exit(2)
		}
		k, err := newK8sClient()
		dieIf(err)
		runtimeClass := "kata"
		ttl32 := int32(*ttl)
		scriptText := strings.TrimSpace(*script)
		aj := &workspacesv1alpha1.AgentJob{
			ObjectMeta: metav1.ObjectMeta{
				Name:      strings.TrimSpace(*name),
				Namespace: strings.TrimSpace(*namespace),
			},
			Spec: workspacesv1alpha1.AgentJobSpec{
				Image:                   strings.TrimSpace(*image),
				Script:                  scriptText,
				Workdir:                 strings.TrimSpace(*workdir),
				PolicyProfile:           strings.TrimSpace(*profile),
				RuntimeClassName:        &runtimeClass,
				TTLSecondsAfterFinished: &ttl32,
				NodeSelector:            map[string]string{"workspaces.platform.dev/pool": "agents"},
				Tolerations: []corev1.Toleration{{
					Key:      "workspaces.platform.dev/pool",
					Operator: corev1.TolerationOpEqual,
					Value:    "agents",
					Effect:   corev1.TaintEffectNoSchedule,
				}},
			},
		}
		if scriptText == "" {
			aj.Spec.Command = []string{"/bin/bash", "-lc"}
			aj.Spec.Args = []string{strings.TrimSpace(*shell)}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dieIf(k.Create(ctx, aj))
		fmt.Printf("created AgentJob %s/%s\n", aj.Namespace, aj.Name)

	case "run-pr":
		fs := flag.NewFlagSet("agent run-pr", flag.ExitOnError)
		var (
			repo    = fs.String("repo", "", "GitHub repo owner/repo")
			pr      = fs.Int("pr", 0, "PR number")
			profile = fs.String("policy-profile", "", "Policy profile (optional)")
			ttl     = fs.Int("ttl", 0, "TTL seconds after finished (optional)")
			broker  = fs.String("broker", getenv("WORKSPACES_BROKER_URL", ""), "Broker base URL")
		)
		fs.Parse(args[1:])
		if strings.TrimSpace(*repo) == "" || *pr <= 0 {
			fs.Usage()
			os.Exit(2)
		}
		tokenHeader, token := brokerAuthForWebhookOrAdmin()
		if token == "" {
			die("missing BROKER_WEBHOOK_TOKEN (preferred) or BROKER_ADMIN_TOKEN")
		}
		if strings.TrimSpace(*broker) == "" {
			die("missing WORKSPACES_BROKER_URL (or --broker)")
		}
		payload := map[string]any{
			"github": map[string]any{
				"repo":       strings.TrimSpace(*repo),
				"pullNumber": *pr,
			},
		}
		if strings.TrimSpace(*profile) != "" {
			payload["policyProfile"] = strings.TrimSpace(*profile)
		}
		if *ttl > 0 {
			payload["ttlSecondsAfterFinished"] = *ttl
		}
		_, err := brokerPostJSON(context.Background(), *broker+"/v1/agent-jobs", tokenHeader, token, payload)
		dieIf(err)
		fmt.Printf("requested agent job for %s#%d\n", strings.TrimSpace(*repo), *pr)

	case "get":
		fs := flag.NewFlagSet("agent get", flag.ExitOnError)
		name := fs.String("name", "", "AgentJob name")
		namespace := fs.String("namespace", "agents", "Namespace")
		fs.Parse(args[1:])
		if strings.TrimSpace(*name) == "" {
			fs.Usage()
			os.Exit(2)
		}
		k, err := newK8sClient()
		dieIf(err)
		var aj workspacesv1alpha1.AgentJob
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dieIf(k.Get(ctx, client.ObjectKey{Namespace: *namespace, Name: *name}, &aj))
		out, _ := json.MarshalIndent(aj, "", "  ")
		fmt.Println(string(out))

	case "list":
		fs := flag.NewFlagSet("agent list", flag.ExitOnError)
		namespace := fs.String("namespace", "agents", "Namespace")
		fs.Parse(args[1:])
		k, err := newK8sClient()
		dieIf(err)
		var list workspacesv1alpha1.AgentJobList
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dieIf(k.List(ctx, &list, client.InNamespace(*namespace)))
		for _, j := range list.Items {
			fmt.Printf("%s/%s phase=%s job=%s\n", j.Namespace, j.Name, j.Status.Phase, j.Status.JobName)
		}

	case "delete":
		fs := flag.NewFlagSet("agent delete", flag.ExitOnError)
		name := fs.String("name", "", "AgentJob name")
		namespace := fs.String("namespace", "agents", "Namespace")
		fs.Parse(args[1:])
		if strings.TrimSpace(*name) == "" {
			fs.Usage()
			os.Exit(2)
		}
		k, err := newK8sClient()
		dieIf(err)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dieIf(k.Delete(ctx, &workspacesv1alpha1.AgentJob{ObjectMeta: metav1.ObjectMeta{Name: *name, Namespace: *namespace}}))
		fmt.Printf("deleted AgentJob %s/%s\n", *namespace, *name)

	default:
		usage()
		os.Exit(2)
	}
}

func cmdNetgrant(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "request":
		fs := flag.NewFlagSet("netgrant request", flag.ExitOnError)
		var (
			namespace   = fs.String("namespace", "agents", "Namespace")
			selector    = fs.String("selector", "", "Pod selector matchLabels (k=v,k2=v2)")
			purpose     = fs.String("purpose", "", "Purpose (required)")
			ttl         = fs.Int("ttl", 1800, "TTL seconds")
			allowNon443 = fs.Bool("allow-non-443", false, "Allow non-443 ports")
			egress      multiFlag
			broker      = fs.String("broker", getenv("WORKSPACES_BROKER_URL", ""), "Broker base URL")
			ghRepo      = fs.String("github-repo", "", "Optional GitHub repo owner/repo for PR workflow")
			ghPR        = fs.Int("github-pr", 0, "Optional PR number for PR workflow")
		)
		fs.Var(&egress, "egress", "Egress rule host[:port] (repeatable)")
		fs.Parse(args[1:])

		if strings.TrimSpace(*broker) == "" {
			die("missing WORKSPACES_BROKER_URL (or --broker)")
		}
		if strings.TrimSpace(*selector) == "" || strings.TrimSpace(*purpose) == "" {
			fs.Usage()
			os.Exit(2)
		}
		if len(egress) == 0 {
			die("at least one --egress is required")
		}

		agentToken := strings.TrimSpace(os.Getenv("BROKER_AGENT_TOKEN"))
		if agentToken == "" {
			die("missing BROKER_AGENT_TOKEN")
		}

		sel, err := parseLabelMap(*selector)
		dieIf(err)

		rules, err := parseEgressRules(egress)
		dieIf(err)

		payload := map[string]any{
			"namespace":   strings.TrimSpace(*namespace),
			"podSelector": sel,
			"policyMode":  "STRICT_FQDN",
			"protocol":    "TCP",
			"purpose":     strings.TrimSpace(*purpose),
			"egress":      rules,
			"ttlSeconds":  *ttl,
			"allowNon443": *allowNon443,
		}
		if strings.TrimSpace(*ghRepo) != "" && *ghPR > 0 {
			payload["github"] = map[string]any{"repo": strings.TrimSpace(*ghRepo), "pullNumber": *ghPR}
		}

		resp, err := brokerPostJSON(context.Background(), *broker+"/v1/network-grants", "X-Broker-Agent-Token", agentToken, payload)
		dieIf(err)
		fmt.Println(string(resp))

	case "approve":
		fs := flag.NewFlagSet("netgrant approve", flag.ExitOnError)
		var (
			namespace  = fs.String("namespace", "agents", "Namespace")
			name       = fs.String("name", "", "NetworkGrant name")
			approvedBy = fs.String("approved-by", "", "ApprovedBy (required)")
			ttl        = fs.Int("ttl", 0, "TTL seconds (optional)")
			reason     = fs.String("reason", "", "Reason (optional)")
			broker     = fs.String("broker", getenv("WORKSPACES_BROKER_URL", ""), "Broker base URL")
		)
		fs.Parse(args[1:])
		if strings.TrimSpace(*broker) == "" || strings.TrimSpace(*name) == "" || strings.TrimSpace(*approvedBy) == "" {
			fs.Usage()
			os.Exit(2)
		}
		adminToken := strings.TrimSpace(os.Getenv("BROKER_ADMIN_TOKEN"))
		if adminToken == "" {
			die("missing BROKER_ADMIN_TOKEN")
		}
		payload := map[string]any{"approvedBy": strings.TrimSpace(*approvedBy)}
		if strings.TrimSpace(*reason) != "" {
			payload["reason"] = strings.TrimSpace(*reason)
		}
		if *ttl > 0 {
			payload["ttlSeconds"] = *ttl
		}
		u := fmt.Sprintf("%s/v1/network-grants/%s/%s/approve", strings.TrimSpace(*broker), strings.TrimSpace(*namespace), strings.TrimSpace(*name))
		resp, err := brokerPostJSON(context.Background(), u, "X-Broker-Admin-Token", adminToken, payload)
		dieIf(err)
		fmt.Println(string(resp))

	default:
		usage()
		os.Exit(2)
	}
}

func cmdGitHub(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "open-pr":
		fs := flag.NewFlagSet("github open-pr", flag.ExitOnError)
		var (
			repo      = fs.String("repo", "", "owner/repo")
			base      = fs.String("base", "", "base branch (optional)")
			title     = fs.String("title", "", "PR title")
			body      = fs.String("body", "", "PR body (optional)")
			patchFile = fs.String("patch-file", "", "Path to unified diff")
			broker    = fs.String("broker", getenv("WORKSPACES_BROKER_URL", ""), "Broker base URL")
		)
		fs.Parse(args[1:])
		if strings.TrimSpace(*broker) == "" || strings.TrimSpace(*repo) == "" || strings.TrimSpace(*title) == "" || strings.TrimSpace(*patchFile) == "" {
			fs.Usage()
			os.Exit(2)
		}

		agentToken := strings.TrimSpace(os.Getenv("BROKER_AGENT_TOKEN"))
		if agentToken == "" {
			die("missing BROKER_AGENT_TOKEN")
		}

		patchBytes, err := os.ReadFile(*patchFile)
		dieIf(err)
		payload := map[string]any{
			"repo":  strings.TrimSpace(*repo),
			"title": strings.TrimSpace(*title),
			"body":  *body,
			"patch": string(patchBytes),
		}
		if strings.TrimSpace(*base) != "" {
			payload["base"] = strings.TrimSpace(*base)
		}

		resp, err := brokerPostJSON(context.Background(), strings.TrimSpace(*broker)+"/v1/github/open-pr", "X-Broker-Agent-Token", agentToken, payload)
		dieIf(err)
		fmt.Println(string(resp))

	default:
		usage()
		os.Exit(2)
	}
}

func newK8sClient() (client.Client, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
	restCfg, err := cfg.ClientConfig()
	if err != nil {
		return nil, err
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = workspacesv1alpha1.AddToScheme(scheme)

	return client.New(restCfg, client.Options{Scheme: scheme})
}

func brokerAuthForWebhookOrAdmin() (header, token string) {
	if v := strings.TrimSpace(os.Getenv("BROKER_WEBHOOK_TOKEN")); v != "" {
		return "X-Broker-Webhook-Token", v
	}
	if v := strings.TrimSpace(os.Getenv("BROKER_ADMIN_TOKEN")); v != "" {
		return "X-Broker-Admin-Token", v
	}
	return "", ""
}

func brokerPostJSON(ctx context.Context, url, tokenHeader, token string, payload any) ([]byte, error) {
	b, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if tokenHeader != "" && token != "" {
		req.Header.Set(tokenHeader, token)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http %s: status=%d body=%s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func parseLabelMap(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" || strings.TrimSpace(kv[1]) == "" {
			return nil, fmt.Errorf("invalid selector part %q (expected k=v)", part)
		}
		out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty selector")
	}
	return out, nil
}

func parseEgressRules(items []string) ([]map[string]any, error) {
	var rules []map[string]any
	for _, it := range items {
		host := strings.TrimSpace(it)
		if host == "" {
			continue
		}
		port := int32(443)
		if strings.Contains(host, ":") {
			parts := strings.Split(host, ":")
			if len(parts) != 2 {
				return nil, fmt.Errorf("invalid egress %q", it)
			}
			host = strings.TrimSpace(parts[0])
			p := strings.TrimSpace(parts[1])
			n, err := strconv.Atoi(p)
			if err != nil || n <= 0 || n > 65535 {
				return nil, fmt.Errorf("invalid port in egress %q", it)
			}
			port = int32(n)
		}
		rules = append(rules, map[string]any{"host": host, "ports": []int32{port}})
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("no valid egress rules")
	}
	return rules, nil
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func dieIf(err error) {
	if err != nil {
		die(err.Error())
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

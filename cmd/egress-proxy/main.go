package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
	auditpkg "workspaces-platform/internal/audit"
	"workspaces-platform/internal/netutil"
)

const (
	labelApp      = "workspaces.platform.dev/app"
	labelAgentJob = "workspaces.platform.dev/agentjob"
)

type podInfo struct {
	Namespace string
	Name      string
	Labels    map[string]string
	PodIP     string
}

type podIndex struct {
	mu   sync.RWMutex
	byIP map[string]podInfo
}

func (p *podIndex) get(ip string) (podInfo, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pi, ok := p.byIP[ip]
	return pi, ok
}

func (p *podIndex) setAll(m map[string]podInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.byIP = m
}

type allowSet map[string]map[int32]struct{} // host -> ports

func (a allowSet) add(host string, port int32) {
	host = netutil.NormalizeHostname(host)
	if host == "" || port <= 0 || port > 65535 {
		return
	}
	ps, ok := a[host]
	if !ok {
		ps = map[int32]struct{}{}
		a[host] = ps
	}
	ps[port] = struct{}{}
}

func (a allowSet) allows(host string, port int32) bool {
	host = netutil.NormalizeHostname(host)
	if host == "" || port <= 0 || port > 65535 {
		return false
	}
	ps, ok := a[host]
	if !ok {
		return false
	}
	_, ok = ps[port]
	return ok
}

type grantIndex struct {
	mu sync.RWMutex

	byJob     map[string]allowSet
	selectors []selectorGrant
}

type selectorGrant struct {
	matchLabels map[string]string
	allow       allowSet
}

func labelsMatchAll(podLabels, required map[string]string) bool {
	if len(required) == 0 {
		return false
	}
	for k, v := range required {
		if strings.TrimSpace(podLabels[k]) != v {
			return false
		}
	}
	return true
}

func (g *grantIndex) allows(jobName string, podLabels map[string]string, host string, port int32) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	as, ok := g.byJob[jobName]
	if !ok {
		as = nil
	}
	if as != nil && as.allows(host, port) {
		return true
	}
	for _, sg := range g.selectors {
		if labelsMatchAll(podLabels, sg.matchLabels) && sg.allow.allows(host, port) {
			return true
		}
	}
	return false
}

func (g *grantIndex) setAll(m map[string]allowSet, selectors []selectorGrant) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.byJob = m
	g.selectors = selectors
}

type server struct {
	k8s client.Client

	audit auditpkg.Emitter

	pods   *podIndex
	grants *grantIndex

	dialTimeout time.Duration
	dnsTimeout  time.Duration

	// allowPrivateIPs controls whether CONNECT destinations may resolve to
	// private/link-local IPs. Default is false for safety (prevents proxy from
	// becoming a bridge into internal networks via DNS rebinding).
	allowPrivateIPs bool
}

func main() {
	listenAddr := getenv("LISTEN_ADDR", ":8080")
	refreshSeconds := intFromEnv("REFRESH_SECONDS", 5)
	dialTimeoutSeconds := intFromEnv("DIAL_TIMEOUT_SECONDS", 10)
	dnsTimeoutSeconds := intFromEnv("DNS_TIMEOUT_SECONDS", 2)
	allowPrivateIPs := strings.EqualFold(strings.TrimSpace(getenv("ALLOW_PRIVATE_IPS", "false")), "true")
	if refreshSeconds <= 0 {
		refreshSeconds = 5
	}
	if dialTimeoutSeconds <= 0 {
		dialTimeoutSeconds = 10
	}
	if dnsTimeoutSeconds <= 0 {
		dnsTimeoutSeconds = 2
	}

	auditEmitter, err := auditpkg.NewFromEnv("egress-proxy")
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

	s := &server{
		k8s:         k8sClient,
		audit:       auditEmitter,
		pods:        &podIndex{byIP: map[string]podInfo{}},
		grants:      &grantIndex{byJob: map[string]allowSet{}, selectors: nil},
		dialTimeout: time.Duration(dialTimeoutSeconds) * time.Second,
		dnsTimeout:  time.Duration(dnsTimeoutSeconds) * time.Second,
		// Default is strict: do not allow proxy to dial private IPs.
		allowPrivateIPs: allowPrivateIPs,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go s.refreshPodsLoop(ctx, time.Duration(refreshSeconds)*time.Second)
	go s.refreshGrantsLoop(ctx, time.Duration(refreshSeconds)*time.Second)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", s.handleProxy)

	log.Printf("egress-proxy listening on %s", listenAddr)
	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    8 << 10, // 8KiB
	}
	log.Fatal(srv.ListenAndServe())
}

func (s *server) refreshPodsLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.refreshPodsOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *server) refreshPodsOnce(ctx context.Context) {
	var pods corev1.PodList
	if err := s.k8s.List(ctx, &pods,
		client.InNamespace("agents"),
		client.MatchingLabels{labelApp: "agent"},
	); err != nil {
		log.Printf("refresh pods failed: %v", err)
		return
	}

	m := map[string]podInfo{}
	for i := range pods.Items {
		p := &pods.Items[i]
		ip := strings.TrimSpace(p.Status.PodIP)
		if ip == "" {
			continue
		}
		// Copy labels to avoid unexpected mutation.
		lbls := map[string]string{}
		for k, v := range p.Labels {
			lbls[k] = v
		}
		m[ip] = podInfo{
			Namespace: p.Namespace,
			Name:      p.Name,
			Labels:    lbls,
			PodIP:     ip,
		}
	}
	s.pods.setAll(m)
}

func (s *server) refreshGrantsLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.refreshGrantsOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *server) refreshGrantsOnce(ctx context.Context) {
	var list workspacesv1alpha1.NetworkGrantList
	if err := s.k8s.List(ctx, &list, client.InNamespace("agents")); err != nil {
		log.Printf("refresh networkgrants failed: %v", err)
		return
	}

	now := time.Now()
	byJob := map[string]allowSet{}
	selectors := []selectorGrant{}

	for i := range list.Items {
		ng := &list.Items[i]
		if !grantActive(ng, now) {
			continue
		}

		mode := ng.Spec.PolicyMode
		if mode == "" {
			mode = workspacesv1alpha1.NetworkGrantPolicyModeStrictFQDN
		}
		// The proxy only enforces PROXY_CONNECT grants. STRICT_FQDN grants are
		// enforced by Cilium directly and should not implicitly become proxy grants.
		if mode != workspacesv1alpha1.NetworkGrantPolicyModeProxyConnect {
			continue
		}

		allow := allowSet{}
		for _, r := range ng.Spec.Egress {
			host := netutil.NormalizeHostname(r.Host)
			if host == "" {
				continue
			}
			ports := r.Ports
			if len(ports) == 0 {
				ports = []int32{443}
			}
			for _, p := range ports {
				allow.add(host, p)
			}
		}

		job := ""
		if ng.Spec.AgentJobRef != nil {
			job = strings.TrimSpace(ng.Spec.AgentJobRef.Name)
		}
		if job == "" {
			// Best-effort support for podSelector-based grants (matchLabels only).
			if ng.Spec.PodSelector == nil || len(ng.Spec.PodSelector.MatchExpressions) != 0 || len(ng.Spec.PodSelector.MatchLabels) == 0 {
				continue
			}
			sel := map[string]string{}
			for k, v := range ng.Spec.PodSelector.MatchLabels {
				if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
					continue
				}
				sel[k] = v
			}
			if len(sel) == 0 {
				continue
			}
			selectors = append(selectors, selectorGrant{matchLabels: sel, allow: allow})
			continue
		}

		as, ok := byJob[job]
		if !ok {
			as = allowSet{}
			byJob[job] = as
		}
		// Merge allow set.
		for host, ports := range allow {
			for p := range ports {
				as.add(host, p)
			}
		}
	}

	s.grants.setAll(byJob, selectors)
}

func grantActive(ng *workspacesv1alpha1.NetworkGrant, now time.Time) bool {
	if ng == nil || !ng.Spec.Approved {
		return false
	}
	if !ng.Status.ExpiresAt.IsZero() {
		return now.Before(ng.Status.ExpiresAt.Time)
	}

	ttl := ng.Spec.TTLSeconds
	if ttl <= 0 {
		ttl = 1800
	}

	approvedAt := ng.Status.ApprovedAt.Time
	if approvedAt.IsZero() {
		// Status is best-effort; if it's not set yet, fail open briefly so the
		// UX doesn't require a "wait for controller reconcile" dance.
		approvedAt = now
	}
	return now.Before(approvedAt.Add(time.Duration(ttl) * time.Second))
}

func (s *server) handleProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte("CONNECT only\n"))
		return
	}

	ctx := r.Context()
	remoteIP, err := remoteIPFromAddr(r.RemoteAddr)
	if err != nil {
		s.audit.Emit("egressproxy.connect_denied", map[string]any{
			"remote_addr": r.RemoteAddr,
			"error":       "bad_remote_addr",
		})
		writeHTTPError(w, http.StatusForbidden, "forbidden")
		return
	}

	pod, ok := s.pods.get(remoteIP)
	if !ok {
		// Best-effort: refresh once to avoid "wait for next tick" UX.
		s.refreshPodsOnce(ctx)
		pod, ok = s.pods.get(remoteIP)
	}
	if !ok || strings.TrimSpace(pod.Namespace) != "agents" {
		s.audit.Emit("egressproxy.connect_denied", map[string]any{
			"remote_ip": remoteIP,
			"error":     "unknown_pod",
		})
		writeHTTPError(w, http.StatusForbidden, "forbidden")
		return
	}

	if strings.TrimSpace(pod.Labels[labelApp]) != "agent" {
		s.audit.Emit("egressproxy.connect_denied", map[string]any{
			"remote_ip": remoteIP,
			"pod":       fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
			"error":     "not_agent",
		})
		writeHTTPError(w, http.StatusForbidden, "forbidden")
		return
	}

	job := strings.TrimSpace(pod.Labels[labelAgentJob])
	if job == "" {
		s.audit.Emit("egressproxy.connect_denied", map[string]any{
			"remote_ip": remoteIP,
			"pod":       fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
			"error":     "missing_agentjob_label",
		})
		writeHTTPError(w, http.StatusForbidden, "forbidden")
		return
	}

	host, port, err := parseHostPort(r.Host)
	if err != nil {
		s.audit.Emit("egressproxy.connect_denied", map[string]any{
			"remote_ip": remoteIP,
			"pod":       fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
			"agentjob":  job,
			"dest":      r.Host,
			"error":     "bad_dest",
		})
		writeHTTPError(w, http.StatusBadRequest, "bad destination")
		return
	}
	host = netutil.NormalizeHostname(host)
	if err := validateConnectHost(host); err != nil {
		s.audit.Emit("egressproxy.connect_denied", map[string]any{
			"remote_ip": remoteIP,
			"pod":       fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
			"agentjob":  job,
			"dest_host": host,
			"dest_port": port,
			"error":     "invalid_host",
			"detail":    err.Error(),
		})
		writeHTTPError(w, http.StatusBadRequest, "bad destination")
		return
	}

	if !s.grants.allows(job, pod.Labels, host, port) {
		// Best-effort: refresh once to avoid "wait for next tick" UX.
		s.refreshGrantsOnce(ctx)
	}
	if !s.grants.allows(job, pod.Labels, host, port) {
		s.audit.Emit("egressproxy.connect_denied", map[string]any{
			"remote_ip": remoteIP,
			"pod":       fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
			"agentjob":  job,
			"dest_host": host,
			"dest_port": port,
			"error":     "not_allowed",
		})
		writeHTTPError(w, http.StatusForbidden, "destination not allowed")
		return
	}

	// Dial destination before hijacking; if dial fails, return a normal HTTP error.
	dialCtx, cancel := context.WithTimeout(ctx, s.dialTimeout)
	defer cancel()
	var d net.Dialer

	dialHost := host
	if !s.allowPrivateIPs {
		ip, err := s.resolvePublicIP(dialCtx, host)
		if err != nil {
			s.audit.Emit("egressproxy.connect_denied", map[string]any{
				"remote_ip": remoteIP,
				"pod":       fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
				"agentjob":  job,
				"dest_host": host,
				"dest_port": port,
				"error":     "dest_not_public",
				"detail":    err.Error(),
			})
			writeHTTPError(w, http.StatusForbidden, "destination not allowed")
			return
		}
		dialHost = ip.String()
	}

	dstConn, err := d.DialContext(dialCtx, "tcp", net.JoinHostPort(dialHost, fmt.Sprintf("%d", port)))
	if err != nil {
		s.audit.Emit("egressproxy.connect_failed", map[string]any{
			"remote_ip": remoteIP,
			"pod":       fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
			"agentjob":  job,
			"dest_host": host,
			"dest_port": port,
			"error":     err.Error(),
		})
		writeHTTPError(w, http.StatusBadGateway, "dial failed")
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = dstConn.Close()
		writeHTTPError(w, http.StatusInternalServerError, "hijack not supported")
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		_ = dstConn.Close()
		writeHTTPError(w, http.StatusInternalServerError, "hijack failed")
		return
	}

	start := time.Now()
	_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	var upBytes int64
	var downBytes int64

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(dstConn, clientConn)
		upBytes = n
		_ = dstConn.Close()
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(clientConn, dstConn)
		downBytes = n
		_ = clientConn.Close()
	}()
	wg.Wait()

	s.audit.Emit("egressproxy.connect", map[string]any{
		"remote_ip":   remoteIP,
		"pod":         fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
		"agentjob":    job,
		"dest_host":   host,
		"dest_port":   port,
		"duration_ms": time.Since(start).Milliseconds(),
		"up_bytes":    upBytes,
		"down_bytes":  downBytes,
	})
}

func validateConnectHost(host string) error {
	// Allow clients to use absolute FQDN form ("example.com.") by normalizing
	// before validation.
	host = netutil.NormalizeHostname(host)
	return netutil.ValidateExactHostname(host)
}

func isCGNAT(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	// 100.64.0.0/10 (Carrier-grade NAT): treat as non-public for safety.
	return ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127
}

func isAllowedDialIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if !ip.IsGlobalUnicast() {
		return false
	}
	if ip.IsPrivate() {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if isCGNAT(ip) {
		return false
	}
	return true
}

func (s *server) resolvePublicIP(ctx context.Context, host string) (net.IP, error) {
	ctx, cancel := context.WithTimeout(ctx, s.dnsTimeout)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}

	var firstV6 net.IP
	for _, a := range addrs {
		ip := a.IP
		if !isAllowedDialIP(ip) {
			continue
		}
		// Prefer IPv4 when available; it tends to be more predictable in
		// on-prem environments (dual-stack can be uneven early on).
		if ip.To4() != nil {
			return ip, nil
		}
		if firstV6 == nil {
			firstV6 = ip
		}
	}
	if firstV6 != nil {
		return firstV6, nil
	}
	return nil, fmt.Errorf("no public A/AAAA records")
}

func parseHostPort(dest string) (string, int32, error) {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return "", 0, errors.New("empty destination")
	}
	host, portStr, err := net.SplitHostPort(dest)
	if err != nil {
		return "", 0, err
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return "", 0, errors.New("empty host")
	}
	pn, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil || pn <= 0 || pn > 65535 {
		return "", 0, errors.New("invalid port")
	}
	return host, int32(pn), nil
}

func remoteIPFromAddr(remoteAddr string) (string, error) {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if remoteAddr == "" {
		return "", errors.New("empty")
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return "", errors.New("invalid ip")
	}
	return ip.String(), nil
}

func writeHTTPError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg + "\n"))
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func intFromEnv(k string, def int) int {
	raw := strings.TrimSpace(os.Getenv(k))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

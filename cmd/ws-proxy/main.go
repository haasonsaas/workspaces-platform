package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

const annotationLastActiveAt = "workspaces.platform.dev/last-active-at"

func main() {
	var (
		kubeconfig  string
		kubeContext string
		namespace   string
		serviceName string
		targetPort  int
		timeout     time.Duration
		verbose     bool
	)

	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig (defaults to $KUBECONFIG or ~/.kube/config)")
	flag.StringVar(&kubeContext, "context", "", "Kubeconfig context override")
	flag.StringVar(&namespace, "namespace", "", "Namespace override (otherwise derived from host)")
	flag.StringVar(&serviceName, "service", "", "Service override (otherwise derived from host)")
	flag.IntVar(&targetPort, "target-port", 0, "Override remote pod port (otherwise derived from Service)")
	flag.DurationVar(&timeout, "timeout", 45*time.Second, "Setup timeout for establishing the proxy (does not limit session duration)")
	flag.BoolVar(&verbose, "v", false, "Verbose logging (to stderr)")
	flag.Parse()

	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: ws-proxy [flags] <host> <port>")
		os.Exit(2)
	}

	host := flag.Arg(0)
	portStr := flag.Arg(1)
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		fmt.Fprintf(os.Stderr, "invalid port: %q\n", portStr)
		os.Exit(2)
	}

	if serviceName == "" || namespace == "" {
		svc, ns, perr := parseServiceHost(host)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "unable to parse host %q: %v\n", host, perr)
			os.Exit(2)
		}
		if serviceName == "" {
			serviceName = svc
		}
		if namespace == "" {
			namespace = ns
		}
	}

	setupCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cfg, err := loadKubeConfig(kubeconfig, kubeContext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kubeconfig: %v\n", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "k8s client: %v\n", err)
		os.Exit(1)
	}

	svc, err := clientset.CoreV1().Services(namespace).Get(setupCtx, serviceName, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "get service %s/%s: %v\n", namespace, serviceName, err)
		os.Exit(1)
	}
	if len(svc.Spec.Selector) == 0 {
		fmt.Fprintf(os.Stderr, "service %s/%s has no selector; cannot find backing pod\n", namespace, serviceName)
		os.Exit(1)
	}

	selector := labels.SelectorFromSet(svc.Spec.Selector)
	podList, err := clientset.CoreV1().Pods(namespace).List(setupCtx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		fmt.Fprintf(os.Stderr, "list pods for %s/%s: %v\n", namespace, serviceName, err)
		os.Exit(1)
	}

	pod, err := pickReadyPod(podList.Items)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no ready pod for %s/%s: %v\n", namespace, serviceName, err)
		os.Exit(1)
	}

	// Best-effort: heartbeat the Desktop last-active timestamp (used for autosuspend).
	dyn, _ := dynamic.NewForConfig(cfg)
	_ = touchDesktop(setupCtx, dyn, namespace, serviceName, svc)
	heartbeatEvery := 5 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("WORKSPACES_HEARTBEAT_SECONDS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			heartbeatEvery = time.Duration(n) * time.Second
		}
	}

	remotePort := targetPort
	if remotePort == 0 {
		rp, rerr := resolveServiceTargetPort(svc, int32(port), pod)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "resolve target port: %v\n", rerr)
			os.Exit(1)
		}
		remotePort = int(rp)
	}

	if verbose {
		fmt.Fprintf(os.Stderr, "ws-proxy: service=%s/%s pod=%s remotePort=%d\n", namespace, serviceName, pod.Name, remotePort)
	}

	heartbeatFn := func() {
		// Don't let heartbeats stall the session.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = touchDesktop(ctx, dyn, namespace, serviceName, svc)
	}
	if err := proxyOnce(setupCtx, cfg, clientset, namespace, pod.Name, remotePort, heartbeatEvery, heartbeatFn); err != nil {
		fmt.Fprintf(os.Stderr, "proxy failed: %v\n", err)
		os.Exit(1)
	}
}

func touchDesktop(ctx context.Context, dyn dynamic.Interface, namespace, serviceName string, svc *corev1.Service) error {
	if dyn == nil {
		return nil
	}
	deskName := ""
	if svc != nil && svc.Labels != nil {
		deskName = strings.TrimSpace(svc.Labels["workspaces.platform.dev/desktop"])
	}
	if deskName == "" {
		const (
			prefix = "desktop-"
			suffix = "-ssh"
		)
		if strings.HasPrefix(serviceName, prefix) && strings.HasSuffix(serviceName, suffix) && len(serviceName) > len(prefix)+len(suffix) {
			deskName = serviceName[len(prefix) : len(serviceName)-len(suffix)]
		}
	}
	if deskName == "" {
		return fmt.Errorf("unable to resolve desktop name from service %q", serviceName)
	}

	gvr := schema.GroupVersionResource{Group: "workspaces.platform.dev", Version: "v1alpha1", Resource: "desktops"}

	ts := time.Now().UTC().Format(time.RFC3339Nano)
	patchObj := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{
				annotationLastActiveAt: ts,
			},
		},
	}
	b, _ := json.Marshal(patchObj)
	_, err := dyn.Resource(gvr).Namespace(namespace).Patch(ctx, deskName, types.MergePatchType, b, metav1.PatchOptions{})
	return err
}

func parseServiceHost(host string) (service string, namespace string, _ error) {
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("expected <service>.<namespace>[...], got %q", host)
	}
	return parts[0], parts[1], nil
}

func defaultKubeconfigPath() string {
	if v := os.Getenv("KUBECONFIG"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}

func loadKubeConfig(kubeconfigPath, contextOverride string) (*rest.Config, error) {
	if kubeconfigPath == "" {
		kubeconfigPath = defaultKubeconfigPath()
	}
	loader := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath}
	overrides := &clientcmd.ConfigOverrides{}
	if contextOverride != "" {
		overrides.CurrentContext = contextOverride
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loader, overrides)
	return cc.ClientConfig()
}

func pickReadyPod(pods []corev1.Pod) (*corev1.Pod, error) {
	if len(pods) == 0 {
		return nil, fmt.Errorf("no pods")
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		if isPodReady(p) {
			return p, nil
		}
	}
	return nil, fmt.Errorf("no running ready pod found")
}

func isPodReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func resolveServiceTargetPort(svc *corev1.Service, servicePort int32, pod *corev1.Pod) (int32, error) {
	var sp *corev1.ServicePort
	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].Port == servicePort {
			sp = &svc.Spec.Ports[i]
			break
		}
	}
	if sp == nil {
		// Not found; treat the requested port as the pod port (best effort).
		return servicePort, nil
	}

	// If targetPort isn't set, Kubernetes defaults it to Port.
	if sp.TargetPort.IntVal == 0 && sp.TargetPort.StrVal == "" {
		return sp.Port, nil
	}

	if sp.TargetPort.IntVal != 0 {
		return sp.TargetPort.IntVal, nil
	}

	name := sp.TargetPort.StrVal
	if name == "" {
		return sp.Port, nil
	}

	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.Name == name && p.ContainerPort != 0 {
				return p.ContainerPort, nil
			}
		}
	}
	return 0, fmt.Errorf("service targetPort %q not found on pod %s", name, pod.Name)
}

func proxyOnce(setupCtx context.Context, cfg *rest.Config, clientset *kubernetes.Clientset, namespace, podName string, remotePort int, heartbeatEvery time.Duration, heartbeat func()) error {
	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return err
	}

	req := clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward")

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	errOut := io.Discard
	out := io.Discard

	// ":<remote>" selects a random local port.
	ports := []string{fmt.Sprintf(":%d", remotePort)}
	pf, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, ports, stopCh, readyCh, out, errOut)
	if err != nil {
		return err
	}

	forwardErr := make(chan error, 1)
	go func() { forwardErr <- pf.ForwardPorts() }()

	select {
	case <-readyCh:
		// continue
	case err := <-forwardErr:
		if err == nil {
			return fmt.Errorf("port-forward unexpectedly exited")
		}
		return err
	case <-setupCtx.Done():
		close(stopCh)
		pf.Close()
		return setupCtx.Err()
	}

	fps, err := pf.GetPorts()
	if err != nil {
		close(stopCh)
		pf.Close()
		return err
	}
	if len(fps) != 1 || fps[0].Local == 0 {
		close(stopCh)
		pf.Close()
		return fmt.Errorf("unexpected forwarded ports: %+v", fps)
	}

	conn, err := (&net.Dialer{}).DialContext(setupCtx, "tcp", fmt.Sprintf("127.0.0.1:%d", fps[0].Local))
	if err != nil {
		close(stopCh)
		pf.Close()
		return err
	}
	defer func() { _ = conn.Close() }()

	var stopHeartbeat func()
	if heartbeatEvery > 0 && heartbeat != nil {
		hbCtx, hbCancel := context.WithCancel(context.Background())
		stopHeartbeat = hbCancel
		go func() {
			t := time.NewTicker(heartbeatEvery)
			defer t.Stop()
			for {
				select {
				case <-hbCtx.Done():
					return
				case <-t.C:
					heartbeat()
				}
			}
		}()
	}

	copyErr := make(chan error, 2)
	go func() {
		_, e := io.Copy(conn, os.Stdin)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		copyErr <- e
	}()
	go func() {
		_, e := io.Copy(os.Stdout, conn)
		copyErr <- e
	}()

	// Wait for either direction to finish; then tear everything down.
	<-copyErr

	if stopHeartbeat != nil {
		stopHeartbeat()
	}

	close(stopCh)
	pf.Close()

	// Drain port-forward goroutine.
	select {
	case <-forwardErr:
	case <-time.After(1 * time.Second):
	}

	return nil
}

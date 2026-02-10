package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type doctorMode string

const (
	doctorModeAll       doctorMode = "all"
	doctorModePreflight doctorMode = "preflight"
	doctorModeInstalled doctorMode = "installed"
)

func cmdDoctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	var (
		mode            = fs.String("mode", "all", "Mode: preflight|installed|all")
		timeout         = fs.Duration("timeout", 10*time.Second, "Per-check timeout")
		agentRuntime    = fs.String("agent-runtimeclass", "kata", "Expected RuntimeClass for agents (empty to skip)")
		storageDriver   = fs.String("storage-driver", "driver.longhorn.io", "Expected CSIDriver name (empty to skip)")
		snapshotClass   = fs.String("snapshot-class", "longhorn", "Expected VolumeSnapshotClass name (empty to skip)")
		systemNamespace = fs.String("system-namespace", "workspaces-system", "Control plane namespace")
		agentsNamespace = fs.String("agents-namespace", "agents", "Agents namespace")
		desktopsNS      = fs.String("desktops-namespace", "desktops", "Desktops namespace")
	)
	fs.Parse(args)

	m := doctorMode(strings.ToLower(strings.TrimSpace(*mode)))
	switch m {
	case "", doctorModeAll:
		m = doctorModeAll
	case doctorModePreflight, doctorModeInstalled:
	default:
		die("invalid --mode (expected preflight|installed|all)")
	}

	restCfg, err := newRestConfig()
	dieIf(err)

	dyn, err := dynamic.NewForConfig(restCfg)
	dieIf(err)

	k, err := newK8sClientWithConfig(restCfg)
	dieIf(err)

	fmt.Printf("wsctl doctor: api=%s\n", strings.TrimSpace(restCfg.Host))

	failures := 0
	warnings := 0
	run := func(required bool, name string, fn func(ctx context.Context) (details string, fix string, _ error)) {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()

		details, fix, err := fn(ctx)
		if err == nil {
			fmt.Printf("[PASS] %s", name)
			if strings.TrimSpace(details) != "" {
				fmt.Printf(" (%s)", strings.TrimSpace(details))
			}
			fmt.Println()
			return
		}

		if required {
			failures++
			fmt.Printf("[FAIL] %s: %v\n", name, err)
		} else {
			warnings++
			fmt.Printf("[WARN] %s: %v\n", name, err)
		}
		if strings.TrimSpace(fix) != "" {
			fmt.Printf("  Fix: %s\n", strings.TrimSpace(fix))
		}
	}

	// Preflight: cluster capabilities needed by this platform.
	if m == doctorModeAll || m == doctorModePreflight {
		fmt.Println("\nPreflight")

		run(true, "Cilium CRD (CiliumNetworkPolicy)", func(ctx context.Context) (string, string, error) {
			_, err := getCRD(ctx, dyn, "ciliumnetworkpolicies.cilium.io")
			return "", "Install Cilium (required) and ensure the Cilium CRDs are registered.", err
		})

		run(false, "ValidatingAdmissionPolicy support (optional)", func(ctx context.Context) (string, string, error) {
			// If unsupported, hardened overlays can still be partially applied (quotas, Cilium policies, etc).
			gvr := schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"}
			_, err := dyn.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
			if err != nil {
				return "", "If you want CEL admission guardrails, upgrade the cluster to a version that supports ValidatingAdmissionPolicy.", err
			}
			return "supported", "", nil
		})

		run(true, "CSI snapshot CRDs (VolumeSnapshot*)", func(ctx context.Context) (string, string, error) {
			for _, n := range []string{
				"volumesnapshots.snapshot.storage.k8s.io",
				"volumesnapshotclasses.snapshot.storage.k8s.io",
				"volumesnapshotcontents.snapshot.storage.k8s.io",
			} {
				if _, err := getCRD(ctx, dyn, n); err != nil {
					return "", "Install the external-snapshotter CRDs + snapshot-controller (required for HomeTemplate snapshots).", err
				}
			}
			return "", "", nil
		})

		if strings.TrimSpace(*storageDriver) != "" {
			driverName := strings.TrimSpace(*storageDriver)
			run(true, "CSI driver "+driverName, func(ctx context.Context) (string, string, error) {
				_, err := dyn.Resource(schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "csidrivers"}).
					Get(ctx, driverName, metav1.GetOptions{})
				if err != nil {
					return "", "Install Longhorn (recommended) or configure your CSI driver; expected a CSIDriver named " + driverName + ".", err
				}
				return "installed", "", nil
			})
		}

		if strings.TrimSpace(*agentRuntime) != "" {
			want := strings.TrimSpace(*agentRuntime)
			run(true, "RuntimeClass "+want, func(ctx context.Context) (string, string, error) {
				_, err := dyn.Resource(schema.GroupVersionResource{Group: "node.k8s.io", Version: "v1", Resource: "runtimeclasses"}).
					Get(ctx, want, metav1.GetOptions{})
				if err != nil {
					return "", "Install Kata Containers and create a RuntimeClass named " + want + " (example: k8s/examples/runtimeclass-kata.yaml), or set DEFAULT_AGENT_RUNTIMECLASS.", err
				}
				return "installed", "", nil
			})
		}
	}

	// Installed: verify base manifests + optional storage bits.
	if m == doctorModeAll || m == doctorModeInstalled {
		fmt.Println("\nInstalled")

		run(true, "Namespace "+strings.TrimSpace(*systemNamespace), func(ctx context.Context) (string, string, error) {
			return "", "Apply the base manifests: `kubectl apply -k k8s`.", mustGetNamespace(ctx, k, strings.TrimSpace(*systemNamespace))
		})
		run(true, "Namespace "+strings.TrimSpace(*agentsNamespace), func(ctx context.Context) (string, string, error) {
			return "", "Apply the base manifests: `kubectl apply -k k8s`.", mustGetNamespace(ctx, k, strings.TrimSpace(*agentsNamespace))
		})
		run(true, "Namespace "+strings.TrimSpace(*desktopsNS), func(ctx context.Context) (string, string, error) {
			return "", "Apply the base manifests: `kubectl apply -k k8s`.", mustGetNamespace(ctx, k, strings.TrimSpace(*desktopsNS))
		})

		run(true, "CRDs (workspaces.platform.dev)", func(ctx context.Context) (string, string, error) {
			for _, n := range []string{
				"desktops.workspaces.platform.dev",
				"agentjobs.workspaces.platform.dev",
				"hometemplates.workspaces.platform.dev",
				"networkgrants.workspaces.platform.dev",
				"portshares.workspaces.platform.dev",
			} {
				if _, err := getCRD(ctx, dyn, n); err != nil {
					return "", "Apply the base manifests: `kubectl apply -k k8s`.", err
				}
			}
			return "", "", nil
		})

		sysNS := strings.TrimSpace(*systemNamespace)
		run(true, "Secret capability-broker-secrets (job_jwt_secret)", func(ctx context.Context) (string, string, error) {
			var s corev1.Secret
			if err := k.Get(ctx, client.ObjectKey{Namespace: sysNS, Name: "capability-broker-secrets"}, &s); err != nil {
				return "", "Apply: `kubectl apply -f k8s/examples/capability-broker-secrets.yaml` (edit the placeholder values).", err
			}
			if v := s.Data["job_jwt_secret"]; len(v) == 0 {
				return "", "Ensure Secret workspaces-system/capability-broker-secrets contains a non-empty `job_jwt_secret` key.", fmt.Errorf("missing job_jwt_secret key")
			}
			return "present", "", nil
		})

		run(true, "ConfigMap capability-broker-config", func(ctx context.Context) (string, string, error) {
			var cm corev1.ConfigMap
			if err := k.Get(ctx, client.ObjectKey{Namespace: sysNS, Name: "capability-broker-config"}, &cm); err != nil {
				return "", "Apply the base manifests: `kubectl apply -k k8s`.", err
			}
			mode := strings.ToLower(strings.TrimSpace(cm.Data["broker_network_public_egress_mode"]))
			if mode == "" {
				mode = "deny"
			}
			if mode != "deny" && mode != "allow" {
				return "", "Set capability-broker-config.data.broker_network_public_egress_mode to deny|allow.", fmt.Errorf("invalid broker_network_public_egress_mode=%q", cm.Data["broker_network_public_egress_mode"])
			}
			if raw := strings.TrimSpace(cm.Data["broker_network_profile_overrides"]); raw != "" && raw != "{}" {
				var tmp any
				if err := json.Unmarshal([]byte(raw), &tmp); err != nil {
					return "", "Fix capability-broker-config.data.broker_network_profile_overrides to be valid JSON (or set it to '{}').", fmt.Errorf("invalid broker_network_profile_overrides JSON")
				}
			}
			return "mode=" + mode, "", nil
		})

		run(true, "ConfigMap workspaces-operator-config", func(ctx context.Context) (string, string, error) {
			var cm corev1.ConfigMap
			if err := k.Get(ctx, client.ObjectKey{Namespace: sysNS, Name: "workspaces-operator-config"}, &cm); err != nil {
				return "", "Apply the base manifests: `kubectl apply -k k8s`.", err
			}
			proxyURL := strings.TrimSpace(cm.Data["agent_egress_proxy_url"])
			if proxyURL == "" {
				return "", "Set workspaces-operator-config.data.agent_egress_proxy_url to the egress-proxy Service URL (recommended).", fmt.Errorf("agent_egress_proxy_url is empty")
			}
			return "agent_egress_proxy_url=" + proxyURL, "", nil
		})

		run(true, "Deployment workspaces-operator", func(ctx context.Context) (string, string, error) {
			return checkDeploymentReady(ctx, k, sysNS, "workspaces-operator", "Apply the base manifests: `kubectl apply -k k8s`.")
		})
		run(true, "Deployment capability-broker", func(ctx context.Context) (string, string, error) {
			return checkDeploymentReady(ctx, k, sysNS, "capability-broker", "Apply the base manifests: `kubectl apply -k k8s`.")
		})
		run(true, "Deployment egress-proxy", func(ctx context.Context) (string, string, error) {
			return checkDeploymentReady(ctx, k, sysNS, "egress-proxy", "Apply the base manifests: `kubectl apply -k k8s`.")
		})

		run(true, "Service capability-broker", func(ctx context.Context) (string, string, error) {
			var svc corev1.Service
			if err := k.Get(ctx, client.ObjectKey{Namespace: sysNS, Name: "capability-broker"}, &svc); err != nil {
				return "", "Apply the base manifests: `kubectl apply -k k8s`.", err
			}
			return "port=8080", "", nil
		})

		run(true, "Service egress-proxy", func(ctx context.Context) (string, string, error) {
			var svc corev1.Service
			if err := k.Get(ctx, client.ObjectKey{Namespace: sysNS, Name: "egress-proxy"}, &svc); err != nil {
				return "", "Apply the base manifests: `kubectl apply -k k8s`.", err
			}
			return "port=8080", "", nil
		})

		run(true, "Cilium policy agents-default-deny", func(ctx context.Context) (string, string, error) {
			gvr := schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}
			_, err := dyn.Resource(gvr).Namespace(strings.TrimSpace(*agentsNamespace)).Get(ctx, "agents-default-deny", metav1.GetOptions{})
			if err != nil {
				return "", "Apply: `kubectl apply -f k8s/policies/agents-default-deny.yaml` (or `kubectl apply -k k8s`).", err
			}
			return "present", "", nil
		})

		run(true, "Cilium policy agents-allow-internal-proxies", func(ctx context.Context) (string, string, error) {
			gvr := schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}
			_, err := dyn.Resource(gvr).Namespace(strings.TrimSpace(*agentsNamespace)).Get(ctx, "agents-allow-internal-proxies", metav1.GetOptions{})
			if err != nil {
				return "", "Apply: `kubectl apply -f k8s/policies/agents-allow-internal-proxies.yaml` (or `kubectl apply -k k8s`).", err
			}
			return "present", "", nil
		})

		run(true, "Cilium policy egress-proxy-ingress", func(ctx context.Context) (string, string, error) {
			gvr := schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}
			_, err := dyn.Resource(gvr).Namespace(sysNS).Get(ctx, "egress-proxy-ingress", metav1.GetOptions{})
			if err != nil {
				return "", "Apply the base manifests: `kubectl apply -k k8s`.", err
			}
			return "present", "", nil
		})

		run(false, "Node pools labeled (workspaces.platform.dev/pool)", func(ctx context.Context) (string, string, error) {
			var nodes corev1.NodeList
			if err := k.List(ctx, &nodes); err != nil {
				return "", "", err
			}
			want := map[string]int{"agents": 0, "desktops": 0}
			for i := range nodes.Items {
				n := &nodes.Items[i]
				pool := strings.TrimSpace(n.Labels["workspaces.platform.dev/pool"])
				if pool == "" {
					continue
				}
				if _, ok := want[pool]; ok {
					want[pool]++
				}
			}
			if want["agents"] == 0 || want["desktops"] == 0 {
				return fmt.Sprintf("agents=%d desktops=%d", want["agents"], want["desktops"]),
					"Label (and ideally taint) node pools for agents/desktops: workspaces.platform.dev/pool=agents|desktops (and taints matching the manifests).",
					fmt.Errorf("missing one or more node pools")
			}
			return fmt.Sprintf("agents=%d desktops=%d", want["agents"], want["desktops"]), "", nil
		})

		run(false, "StorageClass longhorn (recommended)", func(ctx context.Context) (string, string, error) {
			var sc storagev1.StorageClass
			if err := k.Get(ctx, client.ObjectKey{Name: "longhorn"}, &sc); err != nil {
				return "", "Install Longhorn and ensure a StorageClass named 'longhorn' exists (or adjust your Desktop/HomeTemplate specs).", err
			}
			return "provisioner=" + strings.TrimSpace(sc.Provisioner), "", nil
		})

		// Storage: snapshot class (optional but recommended when using Longhorn).
		if strings.TrimSpace(*snapshotClass) != "" {
			className := strings.TrimSpace(*snapshotClass)
			run(false, "VolumeSnapshotClass "+className, func(ctx context.Context) (string, string, error) {
				gvr := schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotclasses"}
				u, err := dyn.Resource(gvr).Get(ctx, className, metav1.GetOptions{})
				if err != nil {
					return "", "Apply: `kubectl apply -f k8s/optional/storage/longhorn-volumesnapshotclass.yaml` (or `kubectl apply -k k8s-overlays/complete`).", err
				}
				driver, _, _ := unstructured.NestedString(u.Object, "driver")
				if driver == "" {
					driver = "<unknown>"
				}
				return "driver=" + driver, "", nil
			})
		}

		run(false, "Hardened overlay (ValidatingAdmissionPolicy objects)", func(ctx context.Context) (string, string, error) {
			gvr := schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"}
			_, err := dyn.Resource(gvr).Get(ctx, "workspaces-agents-pod-hardening", metav1.GetOptions{})
			if err != nil {
				return "", "Apply: `kubectl apply -k k8s-overlays/hardened` (optional).", err
			}
			return "present", "", nil
		})

		run(false, "Pod Security Standards labels on namespaces", func(ctx context.Context) (string, string, error) {
			nsNames := []string{strings.TrimSpace(*systemNamespace), strings.TrimSpace(*agentsNamespace), strings.TrimSpace(*desktopsNS)}
			missing := []string{}
			for _, n := range nsNames {
				if n == "" {
					continue
				}
				var nsObj corev1.Namespace
				if err := k.Get(ctx, client.ObjectKey{Name: n}, &nsObj); err != nil {
					continue
				}
				if nsObj.Labels == nil || strings.TrimSpace(nsObj.Labels["pod-security.kubernetes.io/enforce"]) == "" {
					missing = append(missing, n)
				}
			}
			if len(missing) != 0 {
				return "missing=" + strings.Join(missing, ","), "Apply: `kubectl apply -k k8s-overlays/hardened` (optional) to add namespace PSS labels.", fmt.Errorf("missing PSS labels")
			}
			return "present", "", nil
		})
	}

	if failures != 0 {
		fmt.Printf("\nResult: FAIL (%d failed, %d warnings)\n", failures, warnings)
		os.Exit(1)
	}
	if warnings != 0 {
		fmt.Printf("\nResult: OK (%d warnings)\n", warnings)
		return
	}
	fmt.Println("\nResult: OK")
}

func getCRD(ctx context.Context, dyn dynamic.Interface, name string) (*unstructured.Unstructured, error) {
	gvr := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	return dyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
}

func mustGetNamespace(ctx context.Context, k client.Client, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("empty namespace name")
	}
	var ns corev1.Namespace
	if err := k.Get(ctx, client.ObjectKey{Name: strings.TrimSpace(name)}, &ns); err != nil {
		return err
	}
	return nil
}

func checkDeploymentReady(ctx context.Context, k client.Client, namespace, name, fix string) (details string, outFix string, _ error) {
	var dep appsv1.Deployment
	if err := k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &dep); err != nil {
		return "", fix, err
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	avail := dep.Status.AvailableReplicas
	if desired == 0 {
		return fmt.Sprintf("replicas=0 available=%d", avail), fix, fmt.Errorf("scaled to 0")
	}
	if avail < desired {
		return fmt.Sprintf("available=%d desired=%d", avail, desired), fix, fmt.Errorf("not ready")
	}
	return fmt.Sprintf("available=%d", avail), "", nil
}

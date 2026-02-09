package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
			} {
				if _, err := getCRD(ctx, dyn, n); err != nil {
					return "", "Apply the base manifests: `kubectl apply -k k8s`.", err
				}
			}
			return "", "", nil
		})

		sysNS := strings.TrimSpace(*systemNamespace)
		run(true, "Deployment workspaces-operator", func(ctx context.Context) (string, string, error) {
			return checkDeploymentReady(ctx, k, sysNS, "workspaces-operator", "Apply the base manifests: `kubectl apply -k k8s`.")
		})
		run(true, "Deployment capability-broker", func(ctx context.Context) (string, string, error) {
			return checkDeploymentReady(ctx, k, sysNS, "capability-broker", "Apply the base manifests: `kubectl apply -k k8s`.")
		})
		run(true, "Deployment egress-proxy", func(ctx context.Context) (string, string, error) {
			return checkDeploymentReady(ctx, k, sysNS, "egress-proxy", "Apply the base manifests: `kubectl apply -k k8s`.")
		})

		run(false, "Cilium policy agents-default-deny", func(ctx context.Context) (string, string, error) {
			gvr := schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}
			_, err := dyn.Resource(gvr).Namespace(strings.TrimSpace(*agentsNamespace)).Get(ctx, "agents-default-deny", metav1.GetOptions{})
			if err != nil {
				return "", "Apply: `kubectl apply -f k8s/policies/agents-default-deny.yaml` (or `kubectl apply -k k8s`).", err
			}
			return "present", "", nil
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

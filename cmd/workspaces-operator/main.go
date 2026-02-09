package main

import (
	"flag"
	"os"
	"strconv"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
	"workspaces-platform/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(workspacesv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var leaderElectionID string
	var defaultDesktopImage string
	var defaultAgentRuntimeClass string
	var maxGrantTTLSeconds int
	var maxGrantEgressRules int

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true, "Enable leader election for controller manager.")
	flag.StringVar(&leaderElectionID, "leader-election-id", "workspaces-operator.workspaces.platform.dev", "Leader election ID.")
	flag.StringVar(&defaultDesktopImage, "default-desktop-image", getenv("DEFAULT_DESKTOP_IMAGE", "ghcr.io/workspaces-platform/desktop:latest"), "Default desktop image if Desktop.spec.image is empty.")
	flag.StringVar(&defaultAgentRuntimeClass, "default-agent-runtimeclass", getenv("DEFAULT_AGENT_RUNTIMECLASS", "kata"), "Default RuntimeClassName for AgentJobs.")
	flag.IntVar(&maxGrantTTLSeconds, "networkgrant-max-ttl-seconds", intFromEnv("NETWORKGRANT_MAX_TTL_SECONDS", 7200), "Max ttlSeconds allowed for NetworkGrants (0 disables cap).")
	flag.IntVar(&maxGrantEgressRules, "networkgrant-max-egress-rules", intFromEnv("NETWORKGRANT_MAX_EGRESS_RULES", 20), "Max number of egress destinations allowed for NetworkGrants (0 disables cap).")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if maxGrantEgressRules < 0 {
		maxGrantEgressRules = 0
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.DesktopReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		DefaultDesktopImage: defaultDesktopImage,
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create controller", "controller", "Desktop")
		os.Exit(1)
	}

	if err := (&controller.AgentJobReconciler{
		Client:                   mgr.GetClient(),
		Scheme:                   mgr.GetScheme(),
		DefaultAgentRuntimeClass: defaultAgentRuntimeClass,
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create controller", "controller", "AgentJob")
		os.Exit(1)
	}

	if err := (&controller.NetworkGrantReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		MaxTTLSeconds:  clampNonNegativeInt32(maxGrantTTLSeconds),
		MaxEgressRules: maxGrantEgressRules,
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create controller", "controller", "NetworkGrant")
		os.Exit(1)
	}

	if err := (&controller.HomeTemplateReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create controller", "controller", "HomeTemplate")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctrl.Log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func intFromEnv(k string, def int) int {
	raw := getenv(k, "")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

func clampNonNegativeInt32(n int) int32 {
	if n <= 0 {
		return 0
	}
	const maxInt32 = int(^uint32(0) >> 1)
	if n > maxInt32 {
		return int32(maxInt32)
	}
	return int32(n)
}

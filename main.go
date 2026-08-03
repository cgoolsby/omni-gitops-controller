package main

import (
	"context"
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	omniv1 "github.com/cgoolsby/omni-gitops-controller/api/v1alpha1"
	"github.com/cgoolsby/omni-gitops-controller/controllers"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(omniv1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var kubeconfigNamespace string
	var argoCDClusters bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address for metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for health probes.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election.")
	flag.StringVar(&kubeconfigNamespace, "kubeconfig-namespace", "flux-system",
		"Namespace to write cluster kubeconfig Secrets into.")
	flag.BoolVar(&argoCDClusters, "argocd-clusters", false,
		"Write kubeconfig Secrets in ArgoCD cluster Secret format instead of raw kubeconfig.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// ── Omni client ───────────────────────────────────────────────────────────
	// OMNI_ENDPOINT: Omni HTTPS URL, e.g. "https://omni.192.168.1.92.nip.io:32080"
	// OMNI_SERVICE_ACCOUNT_TOKEN: base64 token from `omnictl serviceaccount create`
	omniEndpoint := os.Getenv("OMNI_ENDPOINT")
	if omniEndpoint == "" {
		setupLog.Error(nil, "OMNI_ENDPOINT env var is required")
		os.Exit(1)
	}
	omniToken := os.Getenv("OMNI_SERVICE_ACCOUNT_TOKEN")
	if omniToken == "" {
		setupLog.Error(nil, "OMNI_SERVICE_ACCOUNT_TOKEN env var is required")
		os.Exit(1)
	}

	omniClient, err := controllers.NewOmniClient(context.Background(), omniEndpoint, omniToken)
	if err != nil {
		setupLog.Error(err, "Unable to create Omni client")
		os.Exit(1)
	}

	// ── Controller manager ────────────────────────────────────────────────────
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: server.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "omni-controller.omni.gitops.dev",
	})
	if err != nil {
		setupLog.Error(err, "Unable to start manager")
		os.Exit(1)
	}

	if err = (&controllers.OmniClusterReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		OmniClient:          omniClient,
		Recorder:            mgr.GetEventRecorder("omni-gitops-controller"),
		KubeconfigNamespace: kubeconfigNamespace,
		ArgoCDClusters:      argoCDClusters,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create OmniCluster controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting omni-controller")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Controller exited with error")
		os.Exit(1)
	}
}

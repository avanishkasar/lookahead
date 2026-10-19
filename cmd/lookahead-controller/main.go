// Command lookahead-controller runs the LookaheadPolicy and DemandHint reconcilers.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/avanishkasar/lookahead/api/v1alpha1"
	"github.com/avanishkasar/lookahead/internal/arrivals"
	"github.com/avanishkasar/lookahead/internal/controller"
	"github.com/avanishkasar/lookahead/internal/forecast"
)

func main() {
	var metricsAddr, probeAddr string
	var leaderElect bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to; 0 disables it.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election so only one replica writes pool replicas.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, extv1beta1.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			setupLog.Error(err, "Unable to build scheme")
			os.Exit(1)
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "lookahead-controller.lookahead.avanishkasar.github.io",
		// Claims can number in the tens of thousands; managed fields are never read here.
		Cache: cache.Options{DefaultTransform: cache.TransformStripManagedFields()},
	})
	if err != nil {
		setupLog.Error(err, "Unable to create manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	if err := controller.IndexPoolRef(ctx, mgr); err != nil {
		setupLog.Error(err, "Unable to register field indexes")
		os.Exit(1)
	}
	if err := (&controller.LookaheadPolicyReconciler{
		Client:     mgr.GetClient(),
		Arrivals:   arrivals.NewRecorder(nil),
		Stabilizer: forecast.NewStabilizer(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to set up LookaheadPolicy controller")
		os.Exit(1)
	}
	if err := (&controller.DemandHintReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to set up DemandHint controller")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to add health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to add ready check")
		os.Exit(1)
	}
	setupLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Manager exited with error")
		os.Exit(1)
	}
}

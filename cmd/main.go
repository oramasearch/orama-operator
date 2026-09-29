/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"github.com/oramasearch/orama-operator/internal/controller"
)

var scheme = runtime.NewScheme()

// Keep the pre-rename ID so the old and new Helm releases contend for the same production Lease
// during the scheduled handoff. Changing this ID would allow both managers to reconcile at once.
const leaderElectionID = "orama-cloud-v1-operator.orama.cloud"

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(oramav1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr, controlPlaneAddr, lakeSQLiteImage, lakeDuckDBImage, lakePolarsImage string
	var enableLeaderElection, enableE2EControllers bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe endpoint")
	flag.StringVar(&controlPlaneAddr, "control-plane-bind-address", ":8090", "deployment control-plane endpoint")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "enable leader election")
	flag.StringVar(&lakeSQLiteImage, "lake-sqlite-image", controller.DefaultLakeSQLiteImage,
		"the lake-sqlite image for a Lake's sqlite services when its spec.services.sqliteImage is empty")
	flag.StringVar(&lakeDuckDBImage, "lake-duckdb-image", controller.DefaultLakeDuckDBImage,
		"the lake-duckdb image for a Lake's duckdb services when its spec.services.duckdbImage is empty")
	flag.StringVar(&lakePolarsImage, "lake-polars-image", controller.DefaultLakePolarsImage,
		"the lake-polars image for a Lake's polars services when its spec.services.polarsImage is empty")
	// The e2e kinds are test fixtures. Production runs no e2e suites — that is what a testing
	// environment is for — so their controllers stay unregistered unless a cluster asks, and their
	// CRDs live outside the install chart (chart/e2e-crds). Off by default means the safe posture is
	// the one you get by forgetting. See docs/operations/crd-rollout.md.
	flag.BoolVar(&enableE2EControllers, "enable-e2e-controllers", false,
		"reconcile the e2e test kinds (OramaCloudV1E2ETest, ZettaClusterE2ETest, LakeE2ETest); "+
			"testing environments only, and the cluster needs the chart/e2e-crds CRDs")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	mgrOpts := ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
	}
	// WATCH_NAMESPACE scopes the manager's cache to one namespace — lets an operator run against a
	// SHARED cluster (e.g. a locally-run controller reconciling only a demo namespace) without touching
	// CRs in other namespaces. Empty = watch all namespaces (the in-cluster default).
	if ns := os.Getenv("WATCH_NAMESPACE"); ns != "" {
		mgrOpts.Cache = cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}}
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// A typed clientset for the one thing the controller-runtime client can't do: read a finished Job's
	// pod logs (corev1 GetLogs) so the Zetta e2e reconciler can parse pass/fail/skip counts into status.
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to build kubernetes clientset")
		os.Exit(1)
	}

	if err = (&controller.OramaCloudV1Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "OramaCloudV1")
		os.Exit(1)
	}
	if err = (&controller.OramaZettaClusterReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(), // uncached reader for the elastic-capacity node count (operator#69)
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "OramaZettaCluster")
		os.Exit(1)
	}
	if err = (&controller.ClerkApplicationReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClerkApplication")
		os.Exit(1)
	}
	if err = (&controller.CloudflareDNSRecordReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CloudflareDNSRecord")
		os.Exit(1)
	}
	if err = (&controller.CloudflareKVNamespaceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CloudflareKVNamespace")
		os.Exit(1)
	}
	if err = (&controller.CloudflareR2BucketReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CloudflareR2Bucket")
		os.Exit(1)
	}
	if err = (&controller.CloudflareLoadBalancerReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CloudflareLoadBalancer")
		os.Exit(1)
	}
	if err = (&controller.RabbitMQTopologyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RabbitMQTopology")
		os.Exit(1)
	}
	if err = (&controller.PostgresSchemaReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PostgresSchema")
		os.Exit(1)
	}
	// lake NOTIFYs lake_services when it writes a service; the listener wakes the Lake reconciler
	// then instead of on the next poll (lake#197).
	servicesListener := controller.NewLakeServicesListener(mgr.GetClient())
	if err = mgr.Add(servicesListener); err != nil {
		setupLog.Error(err, "unable to add the lake services listener")
		os.Exit(1)
	}
	if err = (&controller.LakeReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		SQLiteImage:   lakeSQLiteImage,
		DuckDBImage:   lakeDuckDBImage,
		PolarsImage:   lakePolarsImage,
		ServiceEvents: servicesListener.Events,
		APIReader:     mgr.GetAPIReader(),
		Clientset:     clientset,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Lake")
		os.Exit(1)
	}
	if err = (&controller.LakeEnvironmentReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LakeEnvironment")
		os.Exit(1)
	}
	if err = (&controller.LakeAppReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LakeApp")
		os.Exit(1)
	}
	appsListener := controller.NewLakeAppsListener(mgr.GetClient())
	if err = mgr.Add(appsListener); err != nil {
		setupLog.Error(err, "unable to add the lake apps listener")
		os.Exit(1)
	}
	if err = (&controller.LakeAppsReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader(), Events: appsListener.Events,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LakeAppsSync")
		os.Exit(1)
	}
	workspacesListener := controller.NewLakeWorkspacesListener(mgr.GetClient())
	if err = mgr.Add(workspacesListener); err != nil {
		setupLog.Error(err, "unable to add the lake workspaces listener")
		os.Exit(1)
	}
	if err = (&controller.LakeWorkspacesReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		APIReader:         mgr.GetAPIReader(),
		Events:            workspacesListener.Events,
		OperatorNamespace: os.Getenv("POD_NAMESPACE"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Lake")
		os.Exit(1)
	}
	if err = (&controller.LakeRestoreRehearsalReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LakeRestoreRehearsal")
		os.Exit(1)
	}
	if err = (&controller.LakeSealedSecretReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LakeSealedSecret")
		os.Exit(1)
	}
	// e2e kinds — testing environments only. Production reconciles no e2e resources, so these
	// controllers are not registered and their CRDs are not installed.
	if enableE2EControllers {
		setupLog.Info("e2e controllers enabled; this cluster needs the chart/e2e-crds CRDs installed")
		if err = (&controller.OramaCloudV1E2ETestReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "OramaCloudV1E2ETest")
			os.Exit(1)
		}
		if err = (&controller.ZettaClusterE2ETestReconciler{
			Client:    mgr.GetClient(),
			Scheme:    mgr.GetScheme(),
			Clientset: clientset,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "ZettaClusterE2ETest")
			os.Exit(1)
		}
		if err = (&controller.LakeE2ETestReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "LakeE2ETest")
			os.Exit(1)
		}
	}

	if controlPlaneAddr != "" {
		if err = mgr.Add(&controller.DeploymentControlPlane{
			Client:      mgr.GetClient(),
			APIReader:   mgr.GetAPIReader(),
			BindAddress: controlPlaneAddr,
			Log:         ctrl.Log.WithName("deployment-control-plane"),
		}); err != nil {
			setupLog.Error(err, "unable to create deployment control plane")
			os.Exit(1)
		}
	}

	utilruntime.Must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	utilruntime.Must(mgr.AddReadyzCheck("readyz", healthz.Ping))

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

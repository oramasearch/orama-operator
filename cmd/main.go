// SPDX-License-Identifier: MIT

package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"github.com/oramasearch/orama-operator/internal/backup"
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
	// `/manager backup ...` is the same binary run by a Backup's Job (operator#453): one image, one
	// version, and a backup always runs the code of the operator that scheduled it.
	if len(os.Args) > 1 && os.Args[1] == "backup" {
		os.Exit(backup.Main(os.Args[2:], os.Getenv, "/dev/termination-log", os.Stderr))
	}
	var metricsAddr, probeAddr, controlPlaneAddr, lakeSQLiteImage, lakeDuckDBImage, lakePolarsImage string
	var zettaTunnelImage, cloudflaredImage string
	var controlPlaneCertFile, controlPlaneKeyFile string
	var enableLeaderElection bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe endpoint")
	flag.StringVar(&controlPlaneAddr, "control-plane-bind-address", ":8090", "deployment control-plane endpoint")
	flag.StringVar(&controlPlaneCertFile, "control-plane-cert-file", "/var/run/orama/control-plane/tls.crt", "deployment control-plane TLS certificate")
	flag.StringVar(&controlPlaneKeyFile, "control-plane-key-file", "/var/run/orama/control-plane/tls.key", "deployment control-plane TLS private key")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "enable leader election")
	flag.StringVar(&lakeSQLiteImage, "lake-sqlite-image", controller.DefaultLakeSQLiteImage,
		"the lake-sqlite image for a Lake's sqlite services when its spec.services.sqliteImage is empty")
	flag.StringVar(&lakeDuckDBImage, "lake-duckdb-image", controller.DefaultLakeDuckDBImage,
		"the lake-duckdb image for a Lake's duckdb services when its spec.services.duckdbImage is empty")
	flag.StringVar(&lakePolarsImage, "lake-polars-image", controller.DefaultLakePolarsImage,
		"the lake-polars image for a Lake's polars services when its spec.services.polarsImage is empty")
	flag.StringVar(&zettaTunnelImage, "zetta-tunnel-image", controller.DefaultZettaTunnelImage,
		"the zetta-tunnel image every zetta-tunnel role the operator runs uses")
	flag.StringVar(&cloudflaredImage, "cloudflared-image", controller.DefaultCloudflaredImage,
		"the cloudflared image a CloudflareTunnel's connector runs")
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
	// The cache leaves out Helm's release Secrets and every managedFields (memory.go, operator#677).
	// WATCH_NAMESPACE scopes it to one namespace — lets an operator run against a SHARED cluster
	// (e.g. a locally-run controller reconciling only a demo namespace) without touching CRs in
	// other namespaces. Empty = watch all namespaces (the in-cluster default).
	mgrOpts.Cache = managerCacheOptions(os.Getenv("WATCH_NAMESPACE"))
	// The Go runtime's soft memory limit, from the container's (memory.go, principle 8).
	if limit, err := applyGoMemoryLimit(); err != nil {
		setupLog.Error(err, "the Go memory limit is not set")
	} else if limit > 0 {
		setupLog.Info("Go memory limit set from the container's", "bytes", limit)
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
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		APIReader:         mgr.GetAPIReader(),
		OperatorNamespace: os.Getenv("POD_NAMESPACE"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "OramaCloudV1")
		os.Exit(1)
	}
	if err = (&controller.NetworkSandboxReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NetworkSandbox")
		os.Exit(1)
	}
	if err = (&controller.ConnectionReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Connection")
		os.Exit(1)
	}
	if err = (&controller.EgressRuleReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "EgressRule")
		os.Exit(1)
	}
	if err = (&controller.TunnelReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Tunnel")
		os.Exit(1)
	}
	if err = (&controller.TunnelProviderReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "TunnelProvider")
		os.Exit(1)
	}
	if err = (&controller.ZettaTunnelReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Image:             zettaTunnelImage,
		APIReader:         mgr.GetAPIReader(),
		OperatorNamespace: os.Getenv("POD_NAMESPACE"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ZettaTunnel")
		os.Exit(1)
	}
	if err = (&controller.PeerConnectionReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Image:     zettaTunnelImage,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PeerConnection")
		os.Exit(1)
	}
	if err = (&controller.PeerLinkReconciler{
		Client:            mgr.GetClient(),
		APIReader:         mgr.GetAPIReader(),
		Scheme:            mgr.GetScheme(),
		OperatorNamespace: os.Getenv("POD_NAMESPACE"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PeerLink")
		os.Exit(1)
	}
	// Observability (operator#15, #456): the managing cluster's abstract kinds and the pixie
	// provider's concrete kinds run on every cluster; which do anything depends on the resources.
	if err = (&controller.ObservabilityProviderReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ObservabilityProvider")
		os.Exit(1)
	}
	if err = (&controller.ObservabilityHubReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ObservabilityHub")
		os.Exit(1)
	}
	if err = (&controller.ObservabilityAgentReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ObservabilityAgent")
		os.Exit(1)
	}
	if err = (&controller.PixieCloudReconciler{
		Client:            mgr.GetClient(),
		APIReader:         mgr.GetAPIReader(),
		Scheme:            mgr.GetScheme(),
		OperatorNamespace: os.Getenv("POD_NAMESPACE"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PixieCloud")
		os.Exit(1)
	}
	if err = (&controller.PixieVizierReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PixieVizier")
		os.Exit(1)
	}
	if err = (&controller.CloudflareTunnelReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Image:     cloudflaredImage,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CloudflareTunnel")
		os.Exit(1)
	}
	if err = (&controller.OramaZettaClusterReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(), // uncached reader for the elastic-capacity node count (operator#69)
		// Only a CR in the operator's namespace may own the Karpenter install (operator#280).
		OperatorNamespace: os.Getenv("POD_NAMESPACE"),
		// Reads the features Job's `zetta --help` (the egress allowlist file, operator#724).
		Clientset: clientset,
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
	if err = (&controller.EdgeProviderReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "EdgeProvider")
		os.Exit(1)
	}
	if err = (&controller.EdgeZonePolicyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "EdgeZonePolicy")
		os.Exit(1)
	}
	if err = (&controller.EdgeHostPolicyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "EdgeHostPolicy")
		os.Exit(1)
	}
	if err = (&controller.PaymentProviderReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PaymentProvider")
		os.Exit(1)
	}
	if err = (&controller.PaymentWebhookEndpointReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PaymentWebhookEndpoint")
		os.Exit(1)
	}
	if err = (&controller.StripeWebhookEndpointReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("stripewebhookendpoint-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StripeWebhookEndpoint")
		os.Exit(1)
	}
	if err = (&controller.PaymentCatalogReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PaymentCatalog")
		os.Exit(1)
	}
	if err = (&controller.StripeCatalogReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StripeCatalog")
		os.Exit(1)
	}
	if err = (&controller.CloudflareZonePolicyReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		OperatorNamespace: os.Getenv("POD_NAMESPACE"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CloudflareZonePolicy")
		os.Exit(1)
	}
	if err = (&controller.CloudflareHostPolicyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CloudflareHostPolicy")
		os.Exit(1)
	}
	if err = (&controller.CloudflareDNSRecordReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("cloudflarednsrecord-controller"),
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
	if err = (&controller.StatusPageReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StatusPage")
		os.Exit(1)
	}
	if err = (&controller.BackupScheduleReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BackupSchedule")
		os.Exit(1)
	}
	if err = (&controller.BackupReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		APIReader:         mgr.GetAPIReader(),
		OperatorNamespace: os.Getenv("POD_NAMESPACE"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Backup")
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
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		SQLiteImage:       lakeSQLiteImage,
		DuckDBImage:       lakeDuckDBImage,
		PolarsImage:       lakePolarsImage,
		ServiceEvents:     servicesListener.Events,
		APIReader:         mgr.GetAPIReader(),
		Clientset:         clientset,
		OperatorNamespace: os.Getenv("POD_NAMESPACE"),
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
	volumeSnapshotsListener := controller.NewLakeVolumeSnapshotsListener(mgr.GetClient())
	if err = mgr.Add(volumeSnapshotsListener); err != nil {
		setupLog.Error(err, "unable to add the lake volume snapshots listener")
		os.Exit(1)
	}
	if err = (&controller.LakeVolumeSnapshotsReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
		Events:    volumeSnapshotsListener.Events,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LakeVolumeSnapshots")
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
	// Infrastructure (operator#351, #354, #363): the abstract KubernetesCluster, the provider it
	// points at, the concrete AksCluster, and what is installed on a managed cluster. Every
	// cluster's operator runs these: there is no difference between a management cluster's
	// operator and a leaf's, and any cluster can manage others. A cluster
	// with none of these resources reconciles nothing.
	if err = (&controller.KubernetesClusterProviderReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KubernetesClusterProvider")
		os.Exit(1)
	}
	if err = (&controller.KubernetesClusterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KubernetesCluster")
		os.Exit(1)
	}
	if err = (&controller.AksClusterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AksCluster")
		os.Exit(1)
	}
	// What the management cluster puts on a cluster it manages (operator#363): pinned chart
	// releases, and the cluster's own Orama operator.
	if err = (&controller.ClusterHelmReleaseReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClusterHelmRelease")
		os.Exit(1)
	}
	if err = (&controller.OramaOperatorReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "OramaOperator")
		os.Exit(1)
	}
	// Sets (operator#391): Lake and OramaCloudV1 instances installed onto managed clusters from
	// one template, with their versions and settings kept as described.
	if err = (&controller.LakeSetReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		OperatorNamespace: os.Getenv("POD_NAMESPACE"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LakeSet")
		os.Exit(1)
	}
	// Paid Lake orders (operator#634): registered on every cluster like every other kind. It
	// acts only where a LakeOrderSource exists, which is the management cluster.
	if err = (&controller.LakeOrderSourceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LakeOrderSource")
		os.Exit(1)
	}
	if err = (&controller.OramaCloudV1SetReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "OramaCloudV1Set")
		os.Exit(1)
	}
	// The e2e test kinds. Registered on every cluster like every other kind: no cluster's operator
	// differs from another's. A cluster runs an e2e suite only when someone creates one of these
	// resources on it.
	if err = (&controller.OramaCloudV1E2ETestReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "OramaCloudV1E2ETest")
		os.Exit(1)
	}
	if err = (&controller.OramaCloudV1MigrationReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "OramaCloudV1Migration")
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
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Clientset: clientset,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LakeE2ETest")
		os.Exit(1)
	}

	if controlPlaneAddr != "" {
		if err = mgr.Add(&controller.DeploymentControlPlane{
			Client:      mgr.GetClient(),
			APIReader:   mgr.GetAPIReader(),
			BindAddress: controlPlaneAddr,
			CertFile:    controlPlaneCertFile,
			KeyFile:     controlPlaneKeyFile,
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

/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// LakeSpec defines the desired state of Lake.
// +kubebuilder:validation:XValidation:rule="!has(self.payments) || (has(self.host) && size(self.host) > 0)",message="spec.payments needs spec.host: the payment webhook endpoint is https://<host>/billing/stripe/webhook"
// +kubebuilder:validation:XValidation:rule="!has(self.payments) || (has(self.billing) && self.billing.enabled)",message="spec.payments needs spec.billing.enabled: a Lake with billing off takes no payments"
// +kubebuilder:validation:XValidation:rule="!has(self.customDomain) || (has(self.host) && size(self.host) > 0 && has(self.ingressEnabled) && self.ingressEnabled)",message="spec.customDomain needs spec.host and spec.ingressEnabled: the Orama host stays the Lake's alias and the custom host is routed through the Lake's ingress"
// +kubebuilder:validation:XValidation:rule="!has(self.customDomain) || (self.customDomain.host != self.host && (!has(self.apexHost) || self.customDomain.host != self.apexHost))",message="spec.customDomain.host must differ from spec.host and spec.apexHost"
// +kubebuilder:validation:XValidation:rule="!has(self.customDomain) || !has(self.env) || !self.env.exists(e, e.name == 'LAKE_PUBLIC_HOST' || e.name == 'LAKE_HOST_ALIASES' || e.name == 'LAKE_CONTROL_HOST')",message="with spec.customDomain the operator sets LAKE_PUBLIC_HOST, LAKE_HOST_ALIASES and LAKE_CONTROL_HOST: remove them from spec.env"
type LakeSpec struct {
	// Namespace the lake workloads are deployed into. Created if missing.
	// +kubebuilder:default=lake-dev
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Image for the lake binary. Required: there is no production default.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Replicas of the lake Deployment: 1, or 0 to stop it. More is refused (operator#698): lake runs
	// one process per store root (lake docs/deployment/store-volume.md), so a second replica would
	// report Ready while it waits for the root forever and fail every request routed to it.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// PullSecretName, if set, is attached to the lake pods as an imagePullSecret. Needed for a
	// private spec.image (oramasearch/lake is private). The Secret must exist in spec.namespace.
	// +optional
	PullSecretName string `json:"pullSecretName,omitempty"`

	// Port the container listens on; the Service exposes it on 80. Default 4900, lake's own
	// default (--listen 0.0.0.0:4900). The hello-world echo stand-in overrides it to 5678.
	// +kubebuilder:default=4900
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int32 `json:"port,omitempty"`

	// Args passed to the image's entrypoint. Empty runs the image as built.
	// +optional
	Args []string `json:"args,omitempty"`

	// Resources configures the lake container's CPU and memory requests and limits. When omitted,
	// the operator requests 50m CPU and 128Mi memory and limits memory to 512Mi.
	// +kubebuilder:default={requests:{cpu:"50m",memory:"128Mi"},limits:{memory:"512Mi"}}
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// TopologySpreadConstraints overrides the default pod placement constraints. When omitted and
	// Replicas is greater than one, the operator spreads replicas across kubernetes.io/hostname
	// with ScheduleAnyway so a single-node cluster can still schedule the Deployment.
	// +listType=map
	// +listMapKey=topologyKey
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// Store configures the lake process's persistent root. When ExistingClaim is empty, the
	// operator creates lake-<name>-store once from Size, StorageClassName, and AccessModes. Later
	// spec changes never replace or delete that claim. One lake process owns the root at a time, so
	// a Lake runs one replica whatever the access mode (operator#698).
	// +optional
	Store LakeStoreSpec `json:"store,omitempty"`

	// Rollout controls how a pod-template change replaces lake pods (operator#243).
	// +optional
	Rollout LakeRolloutSpec `json:"rollout,omitempty"`

	// Env appends lake-specific environment variables after the operator-managed variables.
	// Operator-managed names take precedence and conflicting entries are ignored.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Agents runs lake's agents (lake#228): the specialized agents of the Agent Library, one
	// Deployment each, and the launches of user-created agents on agent-runtime (lake#231). Every
	// Lake runs them, on the operator's default images unless the spec overrides one.
	// +optional
	Agents LakeAgentsSpec `json:"agents,omitempty"`

	// Apps configures apps-table synchronization, LakeApp runtime delivery and public addressing.
	// +optional
	Apps LakeAppsSpec `json:"apps,omitempty"`

	// Builds runs one isolated rootless BuildKit Job for each queued row in lake's builds table.
	// The operator creates short-lived source, registry-push, and event credentials and reports
	// the Job's observed outcome to the row. See docs/lake/builds.md.
	// +optional
	Builds LakeBuildsSpec `json:"builds,omitempty"`

	// Methodologies runs a workspace's own methodologies, Python code in a workspace repository,
	// as untrusted code (lake#655): each row of lake's methodology_runs table becomes a stage, a
	// sandbox and a collect pod in the Lake's methodology-runs namespace. Needs Builds, which
	// builds each methodology's image. See docs/lake/methodology-runs.md.
	// +optional
	Methodologies LakeMethodologiesSpec `json:"methodologies,omitempty"`

	// Sealing configures the X25519 cluster delivery key. The operator mints generation 1 when
	// omitted. Increasing Generation rotates the public key while retaining private keys still
	// needed by LakeSealedSecret resources.
	// +optional
	Sealing LakeSealingSpec `json:"sealing,omitempty"`

	// Postgres is lake's control plane: a CloudNativePG Cluster the operator provisions in
	// spec.namespace, the same path OramaCloudV1 uses for its control DB (requires the
	// CloudNativePG operator, see the helmfile). CNPG generates the `lake-<name>-controldb-app`
	// Secret (owner credentials + connection URI); the lake pods receive it as
	// LAKE_CONTROL_DB_URI. The Zetta field below configures lake's owned data plane.
	// +optional
	Postgres LakePostgresSpec `json:"postgres,omitempty"`

	// Zetta is lake's data plane: an owned OramaZettaCluster in spec.namespace. Its client
	// Service and seeded admin token are injected into lake as LAKE_DATA_PLANE_URL and
	// LAKE_DATA_PLANE_TOKEN. The child is deleted before the Lake finalizer is released.
	// +optional
	Zetta LakeZettaSpec `json:"zetta,omitempty"`

	// IngressEnabled gates external HTTP exposure of the lake Service. When true AND Host is set, the
	// operator provisions an nginx Ingress at Host (the whole host proxies to the lake Service) and,
	// if DNSSecretRef is also set, a proxied Cloudflare DNS record (Host -> the ingress LoadBalancer).
	// When false (the default) no Ingress or DNS record is created, and any previously created are
	// torn down, even if Host is still set. Host names the endpoint; this bool is the on/off switch.
	// Same contract as OramaZettaCluster.
	// +kubebuilder:default=false
	// +optional
	IngressEnabled bool `json:"ingressEnabled,omitempty"`

	// IngressClassName is the ingress controller that serves this Lake's Ingress and its environments' Ingresses: "nginx"
	// (ingress-nginx, the default until a cluster's cutover) or "zetta-tunnel" (zetta-tunnel's
	// ingress gateway, docs/infrastructure/ingress.md). Changing it moves the hosts' DNS, because DNS
	// follows the Ingress's load-balancer address.
	// +kubebuilder:validation:Enum=nginx;zetta-tunnel
	// +optional
	IngressClassName string `json:"ingressClassName,omitempty"`

	// TLS names a hand-supplied certificate for Host and ApexHost. Every Lake Ingress serves HTTPS
	// whether or not it is set: without it the operator issues the certificates from Let's Encrypt
	// through DNSSecretRef (operator#578, docs/lake/origin-tls.md). It has no effect unless
	// IngressEnabled and Host are also set. The referenced kubernetes.io/tls Secret must exist in
	// spec.namespace.
	// +optional
	TLS LakeTLSSpec `json:"tls,omitempty"`

	// Host is the domain name the lake is exposed at (e.g. hello.lake.live) when IngressEnabled is
	// true. Requires an ingress controller (class nginx).
	// +optional
	Host string `json:"host,omitempty"`

	// DNSSecretRef, if set together with Host, creates a proxied Cloudflare DNS record (Host -> the
	// ingress LoadBalancer) using the Cloudflare credentials (keys `token`, `zoneID`) in the named
	// Secret. The Secret must live in spec.namespace (the CloudflareDNSRecord is created there) and
	// hold the zone ID of Host's zone: a lake.live host needs a lake.live-zone Secret, not the
	// oramadev.net one the Zetta clusters use.
	// +optional
	DNSSecretRef *corev1.LocalObjectReference `json:"dnsSecretRef,omitempty"`

	// PreviewWildcard is the DNS suffix used for per-change preview environments. For example,
	// preview.example.com yields <change-short>.preview.example.com. When Lake ingress and DNS are
	// enabled, the operator maintains *.preview.example.com against the ingress load balancer so
	// individual preview environments do not create Cloudflare records.
	// +optional
	PreviewWildcard string `json:"previewWildcard,omitempty"`

	// Region is where this Lake runs, for people choosing between a deployment's regions
	// (decision 64). A LakeSet fills Name from the member's KubernetesCluster; Label is stated.
	// The Lake receives them as LAKE_REGION and LAKE_REGION_LABEL.
	// +optional
	Region *LakeRegionSpec `json:"region,omitempty"`

	// Regions are every region of the deployment this Lake belongs to, its own included, as its
	// region picker offers them (LAKE_REGIONS, JSON). A LakeSet computes the list from its members
	// and writes it into each one; a set does not state it.
	// +optional
	Regions []LakeRegion `json:"regions,omitempty"`

	// ApexHost is a second host this Lake answers, where it serves the region picker (lake.live in
	// front of eu.lake.live and us.lake.live). The Ingress gets a rule for it and, with
	// DNSSecretRef, an owned proxied DNS record; removing the field removes both. The Lake receives
	// it as LAKE_APEX_HOST. It has no effect without IngressEnabled and Host. At most one member of
	// a LakeSet sets it.
	// +optional
	ApexHost string `json:"apexHost,omitempty"`

	// CustomDomain serves this Lake on a host in a zone the customer owns, white-labelled
	// (orama-operator#947, docs/lake/custom-domains.md). The operator writes no DNS record in the
	// customer's zone: it publishes in status.customDomain the CNAME and the ownership TXT record the
	// customer creates, checks both by DNS lookup, and only then routes the host, with its own
	// Let's Encrypt certificate (HTTP-01) that cert-manager renews. Once the certificate is issued
	// the Lake's public host becomes CustomDomain.Host (LAKE_PUBLIC_HOST); spec.host keeps working
	// as an alias (LAKE_HOST_ALIASES) and stays the Lake's name on the operator's control rows
	// (LAKE_CONTROL_HOST). Removing it removes the route, the certificate and the variables.
	// It needs IngressEnabled and Host.
	// +optional
	CustomDomain *LakeCustomDomainSpec `json:"customDomain,omitempty"`

	// Clerk signs people in through a Clerk instance (operator#541, docs/lake/clerk.md). With the
	// instance's secret key the operator keeps the instance configured for lake through Clerk's
	// Backend API (the `lake` JWT template, the Lake's allowed origins, the webhook endpoint at
	// https://<host>/identity/clerk-webhook), writes a production instance's DNS records with
	// DNSSecretRef, and gives the Lake LAKE_CLERK_PUBLISHABLE_KEY and LAKE_CLERK_WEBHOOK_SECRET.
	// Without it nothing about Clerk is managed, and spec.env keeps whatever it says.
	// +optional
	Clerk *LakeClerkSpec `json:"clerk,omitempty"`

	// Payments takes payments through a payment provider ([orama-bridge#199](https://github.com/oramasearch/orama-bridge/issues/199),
	// docs/lake/payments.md). From the Secret the operator keeps a PaymentProvider, a
	// PaymentWebhookEndpoint at https://<host>/billing/stripe/webhook and Lake's PaymentCatalog, and
	// gives the Lake LAKE_STRIPE_SECRET_KEY and LAKE_STRIPE_WEBHOOK_SECRET. Without it nothing about
	// payments is managed, and spec.env keeps whatever it says.
	// +optional
	Payments *LakePaymentsSpec `json:"payments,omitempty"`

	// Billing turns Lake's billing on: the operator gives the Lake LAKE_BILLING=on (lake
	// docs/api/billing.md). Off, the default, a Lake bills nothing and takes no payments: it
	// registers no billing capabilities, builds no payment provider, and answers its Stripe webhook
	// 404. An on-prem or enterprise install leaves it unset. spec.payments needs it enabled; it can
	// be enabled without spec.payments, for a Lake that bills through the manual provider. Set, it
	// replaces a LAKE_BILLING entry in spec.env; unset, spec.env keeps whatever it says.
	// +optional
	Billing *LakeBillingSpec `json:"billing,omitempty"`

	// DeploySync configures how frequently this operator materializes desired deployment rows from
	// its Zetta orama_control database into LakeEnvironment resources.
	// +optional
	DeploySync LakeDeploySyncSpec `json:"deploySync,omitempty"`

	// Services lets lake run managed services for its customers: one OramaZettaCluster or
	// CloudNativePG Cluster per row of the services table in lake's control Postgres, with the
	// operator's observations written back to service_status. Needs the control-plane Postgres
	// (spec.postgres.enabled); without it condition FeaturesReady is False. See docs/lake/services.md.
	// +optional
	Services LakeServicesSpec `json:"services,omitempty"`

	// Workspaces configures synchronization of lake's workspaces table into isolated Kubernetes
	// namespaces. Each running row gets a Zetta cluster, a restricted namespace profile and local
	// credentials. Needs the control-plane Postgres; without it condition FeaturesReady is False.
	// See docs/lake/workspaces.md.
	// +optional
	Workspaces LakeWorkspacesSpec `json:"workspaces,omitempty"`

	// Snapshots takes scheduled CSI snapshots of lake's store volume and of every workspace Zetta's
	// volumes from lake's volume_snapshot_policies table, keeps the newest of each, and reports
	// them in volume_snapshot_status (operator#260). Requires the control-plane Postgres and the
	// snapshot.storage.k8s.io CRDs. See docs/lake/volume-snapshots.md.
	// +optional
	Snapshots LakeSnapshotsSpec `json:"snapshots,omitempty"`
}

// LakeCustomDomainSpec is a host the customer owns, served by the Lake.
type LakeCustomDomainSpec struct {
	// Host is the customer's host name, for example data.example.com. It must be a name the
	// customer can point a CNAME at: one below their zone's apex, or the apex with a provider that
	// flattens CNAMEs.
	// +kubebuilder:validation:MinLength=4
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`
	Host string `json:"host"`
}

// LakeCustomDomainStatus is the customer domain as the operator last observed it.
type LakeCustomDomainStatus struct {
	// Host is the spec.customDomain.host this status is for.
	Host string `json:"host"`
	// Phase is Pending (the records are not both found yet), Verified (both found; the route and
	// its certificate are being made) or Routed (the host is served with an issued certificate,
	// and is the Lake's public host).
	// +kubebuilder:validation:Enum=Pending;Verified;Routed
	Phase string `json:"phase"`
	// Records are the DNS records the customer creates in their zone: the CNAME that routes the
	// host to this cluster, the TXT record that proves they own it, and any record a Clerk
	// instance on their domain asks for. Found says whether the last lookup saw it.
	// +optional
	Records []LakeCustomerDNSRecord `json:"records,omitempty"`
	// VerifiedTime is when both records were first found for Host. Verification is not undone by
	// a later failed lookup; Records shows what the lookup sees now.
	// +optional
	VerifiedTime *metav1.Time `json:"verifiedTime,omitempty"`
	// CertificateReady is whether the host's certificate is issued and valid.
	// +optional
	CertificateReady bool `json:"certificateReady,omitempty"`
	// CertificateNotAfter is when the issued certificate expires; cert-manager renews it before.
	// +optional
	CertificateNotAfter *metav1.Time `json:"certificateNotAfter,omitempty"`
	// Message says what is missing, in plain words.
	// +optional
	Message string `json:"message,omitempty"`
}

// LakeCustomerDNSRecord is one DNS record the customer creates in their own zone.
type LakeCustomerDNSRecord struct {
	// Purpose is routing (sends the host to this cluster), ownership (proves the customer controls
	// the name) or clerk (a record a Clerk instance on the customer's domain needs).
	// +kubebuilder:validation:Enum=routing;ownership;clerk
	Purpose string `json:"purpose"`
	// Type is CNAME, A, AAAA or TXT.
	Type string `json:"type"`
	// Name is the record's full name.
	Name string `json:"name"`
	// Value is what the record holds: the CNAME target, the address, or the TXT text.
	Value string `json:"value"`
	// Found is whether the operator's last lookup saw the record with this value.
	// +optional
	Found bool `json:"found,omitempty"`
}

// LakeClerkSpec is the Clerk instance a Lake signs people in through.
type LakeClerkSpec struct {
	// SecretKeySecretRef names a Secret in spec.namespace holding the instance's Backend API secret
	// key (sk_test_... or sk_live_...) under key secretKey. The key decides which application and
	// instance this is; Lakes holding the same key share the instance. The Secret may also hold
	// publishableKey, which then replaces the one derived from the instance's Frontend API host.
	// A LakeSet copies it to every member with spec.secrets.
	SecretKeySecretRef corev1.LocalObjectReference `json:"secretKeySecretRef"`
}

// LakeBillingSpec is whether a Lake bills.
type LakeBillingSpec struct {
	// Enabled gives the Lake LAKE_BILLING=on. False gives it no LAKE_BILLING, so billing is off.
	Enabled bool `json:"enabled"`
}

// LakePaymentsSpec is the payment account a Lake bills through.
type LakePaymentsSpec struct {
	// Mode is the Stripe mode this Lake must bill in, written in the committed set file so that a
	// live Secret given to a nonprod Lake (or the reverse) is refused instead of used.
	// +kubebuilder:validation:Enum=test;live
	Mode string `json:"mode"`

	// SecretRef names a Secret in spec.namespace with keys accountID (acct_...), operatorKey (the
	// operator's restricted key) and appKey (Lake's restricted key). Both keys' prefixes must agree
	// with mode. A LakeSet copies it to each member with spec.secrets.
	SecretRef corev1.LocalObjectReference `json:"secretRef"`
}

// LakeRegionSpec is the region a Lake runs in.
type LakeRegionSpec struct {
	// Name is the cloud region (swedencentral). In a LakeSet it comes from the member's cluster
	// and cannot be stated differently.
	// +optional
	Name string `json:"name,omitempty"`
	// Label is the region's name for people ("Europe").
	// +optional
	Label string `json:"label,omitempty"`
}

// LakeRegion is one region of a deployment, as a region picker offers it.
type LakeRegion struct {
	// Name is the cloud region.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Label is the region's name for people.
	// +kubebuilder:validation:MinLength=1
	Label string `json:"label"`
	// Host is the region's Lake host (eu.lake.live).
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`
	// Latitude and Longitude locate the region's datacenter in decimal degrees, so a picker can
	// preselect the region closest to a visitor.
	// +kubebuilder:validation:MinLength=1
	Latitude string `json:"latitude"`
	// +kubebuilder:validation:MinLength=1
	Longitude string `json:"longitude"`
}

// LakeSnapshotsSpec configures the scheduled CSI snapshots of lake's volumes. Every Lake
// reconciles lake's volume_snapshot_policies table; there is no switch (operator#539). Turning a
// policy off is lake's, per target. The operator never deletes a snapshot to stop one.
type LakeSnapshotsSpec struct {
	// ClassName is the VolumeSnapshotClass every snapshot names. The operator creates it when it
	// is absent, for Driver with deletionPolicy Retain, so deleting a snapshot, a workspace or its
	// namespace never deletes the snapshot's contents. An existing class is used as it is, and is
	// refused unless its deletionPolicy is Retain.
	// +kubebuilder:default=lake-retain
	// +kubebuilder:validation:MinLength=1
	// +optional
	ClassName string `json:"className,omitempty"`

	// Driver is the CSI driver of a class the operator creates. It must be the driver of the
	// volumes it snapshots.
	// +kubebuilder:default=disk.csi.azure.com
	// +optional
	Driver string `json:"driver,omitempty"`
}

// LakeAppsSpec configures the shared settings used by LakeApp resources. Every Lake synchronizes
// lake's apps table into LakeApp resources; there is no switch (operator#539).
type LakeAppsSpec struct {
	// Wildcard is the DNS suffix for apps. An app without spec.host uses <app>.<wildcard>. It has
	// no default, and it must be a registrable domain of its own: one equal to, a parent of or a
	// child of the Lake's host, public host, apex host or preview wildcard, or under the same
	// registrable domain as any of them, is refused (condition AppsWildcardReady, reason
	// AppsWildcardSharesLakeSite), because an app could then set cookies for Lake (#762). Empty or
	// refused, no app is served (reason AppsWildcardUnset or AppsWildcardSharesLakeSite). lake
	// receives it as LAKE_APPS_WILDCARD, and with DNSSecretRef the operator keeps the
	// *.<wildcard> DNS record.
	// +optional
	Wildcard string `json:"wildcard,omitempty"`

	// IngressClass selects the ingress controller for apps. Empty defaults to nginx. Apps stay on nginx
	// through the zetta-tunnel cutover: the gateway does not make nginx's auth subrequest to Lake or
	// count bytes per Ingress (docs/infrastructure/ingress.md).
	// +optional
	IngressClass string `json:"ingressClass,omitempty"`

	// RuntimeImage is the private app-runtime image used by runtime-mode apps. Empty uses the
	// operator's default (internal/controller/lake_defaults.go).
	// +optional
	RuntimeImage string `json:"runtimeImage,omitempty"`
}

// LakeBuildsSpec configures lake's in-cluster image builds. Every Lake polls lake's builds table;
// there is no switch (operator#539).
type LakeBuildsSpec struct {
	// BuilderImage is the private oramasearch/lake-builder image run for every build. Empty uses
	// the operator's default (internal/controller/lake_defaults.go). The operator copies the Lake's
	// pullSecretName into each build namespace and uses that local copy to pull it.
	// +optional
	BuilderImage string `json:"builderImage,omitempty"`

	// Resources configures each builder container. Values are merged over the operator's bounded
	// CPU, memory, and ephemeral-storage defaults.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Concurrency caps active Jobs for this Lake and for any one target registry space.
	// +optional
	Concurrency LakeBuildConcurrencySpec `json:"concurrency,omitempty"`

	// CacheEnabled exports and imports a per-space registry cache. The default is true.
	// +kubebuilder:default=true
	// +optional
	CacheEnabled *bool `json:"cacheEnabled,omitempty"`
}

// LakeMethodologiesSpec configures runs of workspace methodologies.
type LakeMethodologiesSpec struct {
	// RuntimeImage is the private oramasearch/methodology-runtime image. Lake builds each
	// methodology's image FROM it (LAKE_METHODOLOGY_RUNTIME_IMAGE), and the stage and collect
	// pods run it unchanged. Empty uses the operator's default (internal/controller/lake_defaults.go).
	// +optional
	RuntimeImage string `json:"runtimeImage,omitempty"`

	// WorkVolumeSize is the size of each run's work volume, which carries the data in and the
	// result and artifact out. Default 10Gi.
	// +optional
	WorkVolumeSize *resource.Quantity `json:"workVolumeSize,omitempty"`

	// StorageClassName is the work volume's storage class; empty uses the cluster default. A GPU
	// run's volume is placed in a zone where its GPU class has nodes by pinning the run's first pod
	// there, so the class should bind volumes on first use (WaitForFirstConsumer).
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// MaxConcurrentRunsPerWorkspace is how many of one workspace's runs may be staging, running
	// or collecting at once. Runs past it wait as Queued, oldest first, and Lake shows why.
	// Default 2.
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxConcurrentRunsPerWorkspace int32 `json:"maxConcurrentRunsPerWorkspace,omitempty"`

	// NamespaceQuota is the hard limit of the ResourceQuota on the Lake's shared methodology-runs
	// namespace: a ceiling on every workspace's runs and served models together, beside each
	// workspace's own quota, which they count against (orama-operator#799). Empty uses the
	// operator's default (requests.cpu 64, requests.memory and limits.memory 256Gi,
	// requests.nvidia.com/gpu 8, pods 500, persistentvolumeclaims 200).
	// +optional
	NamespaceQuota corev1.ResourceList `json:"namespaceQuota,omitempty"`
}

// LakeBuildConcurrencySpec sets independent build concurrency limits.
type LakeBuildConcurrencySpec struct {
	// PerLake is the maximum number of active build Jobs for this Lake.
	// +kubebuilder:default=4
	// +kubebuilder:validation:Minimum=1
	// +optional
	PerLake int32 `json:"perLake,omitempty"`

	// PerSpace is the maximum number of active build Jobs targeting one registry space.
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +optional
	PerSpace int32 `json:"perSpace,omitempty"`
}

// LakeServicesSpec configures a Lake's managed services (liquid-bridge decision 46). Every Lake
// runs them, with no switch (operator#539): the operator reads the services table from lake's
// control Postgres, creates <lake>-<slug> in the Lake's namespace for each present service, deletes
// absent ones, writes service_status, and mounts Secret lake-<lake>-services in lake at
// /var/run/lake/services (LAKE_SERVICE_DIR).
type LakeServicesSpec struct {
	// MaxServices caps the live services of this Lake. A new service beyond the cap is not created
	// and is reported Failed with the message "service quota reached (<n>)"; it starts once there
	// is room. Lowering the cap never removes or refuses an existing service. 0 means unlimited.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxServices int32 `json:"maxServices,omitempty"`

	// ZettaImage is the image for Zetta services and workspace Zettas. Empty falls back to
	// spec.workspaces.image, and then to the operator's default (internal/controller/lake_defaults.go).
	// +optional
	ZettaImage string `json:"zettaImage,omitempty"`

	// ZettaEnv is copied to every managed Zetta service's and workspace Zetta's OramaZettaCluster
	// spec.env, followed by the operator's default Zetta env for every name it does not set
	// (ZETTA_MALLOC_TRIM_THRESHOLD_BYTES=67108864, MALLOC_ARENA_MAX=2, and ZETTA_EGRESS_ALLOW rendered
	// from what the Zetta's workspace NetworkSandbox allows it). ZETTA_CONNECTOR_ALLOW_DESTINATIONS is
	// always the operator's: the exact host:port of each postgres, tidb, sqlite, duckdb and polars
	// service of the Zetta's workspace namespace, and a value stated here is dropped. Operator-managed
	// Zetta environment names remain reserved and are reported by each child cluster when ignored.
	// +optional
	ZettaEnv []corev1.EnvVar `json:"zettaEnv,omitempty"`

	// SQLiteImage is the lake-sqlite image for sqlite services, which the operator runs itself
	// (one StatefulSet per service). Empty falls back to the operator's --lake-sqlite-image flag,
	// which the chart sets from lakeServices.sqliteImage. Changing it rolls every sqlite service's
	// pod. See docs/lake/lake-sqlite.md.
	// +optional
	SQLiteImage string `json:"sqliteImage,omitempty"`

	// DuckDBImage is the lake-duckdb image for duckdb services, which the operator runs itself
	// (one StatefulSet of one pod per service). Empty falls back to the operator's
	// --lake-duckdb-image flag, which the chart sets from lakeServices.duckdbImage. Changing it
	// rolls every duckdb service's pod. See docs/lake/lake-duckdb.md.
	// +optional
	DuckDBImage string `json:"duckdbImage,omitempty"`

	// PolarsImage is the lake-polars image for polars services, which the operator runs itself
	// (one StatefulSet of spec.instances workers per service). Empty falls back to the operator's
	// --lake-polars-image flag, which the chart sets from lakeServices.polarsImage. Changing it
	// rolls every polars service's pods. See docs/lake/lake-polars.md.
	// +optional
	PolarsImage string `json:"polarsImage,omitempty"`
}

// LakeWorkspacesSpec configures isolated workspaces reconciled from lake's control Postgres. Every
// Lake synchronizes lake's workspaces table; there is no switch (operator#539).
type LakeWorkspacesSpec struct {
	// Image is the Zetta image used by every workspace when spec.services.zettaImage is empty. Empty
	// uses the operator's default (internal/controller/lake_defaults.go).
	// +optional
	Image string `json:"image,omitempty"`

	// Storage configures every workspace Zetta cluster's persistent data volume. StorageClassName is
	// the class of every workspace volume. Size is deprecated and ignored once lake's schema has
	// workspaces.storage_gi (migration 0069): each workspace's size comes from that column, whose
	// default for new workspaces is lake's (5 GiB); Size applies only to a row without one.
	// +kubebuilder:default={size:"5Gi"}
	// +optional
	Storage OramaZettaStorage `json:"storage,omitempty"`

	// Resources configures each workspace Zetta node's container. When omitted, the operator
	// requests 64Mi memory, limits memory to 1Gi, and sets no CPU request.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// MaxWorkspaces caps running and stopped workspace namespaces. Existing workspaces are never
	// removed when this value is lowered. Zero means unlimited.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxWorkspaces int32 `json:"maxWorkspaces,omitempty"`

	// DefaultPlan selects the quota plan for a workspace without an override. Empty selects the
	// built-in "default" plan.
	// +optional
	DefaultPlan string `json:"defaultPlan,omitempty"`

	// Plans defines named ResourceQuota and LimitRange profiles. The built-in "default" plan is
	// always available and can be replaced by defining a plan with that name.
	// +optional
	Plans map[string]LakeWorkspacePlanSpec `json:"plans,omitempty"`

	// Overrides selects a plan and outbound internet policy by canonical workspace UUID. A missing
	// override uses DefaultPlan and denies internet egress. Internet access is limited to TCP 443.
	// +optional
	Overrides map[string]LakeWorkspaceOverrideSpec `json:"overrides,omitempty"`
}

// LakeWorkspacePlanSpec defines one namespace quota and its per-container defaults.
type LakeWorkspacePlanSpec struct {
	// Quota is the hard ResourceQuota applied to a workspace namespace.
	// +optional
	Quota corev1.ResourceList `json:"quota,omitempty"`

	// DefaultRequest is the LimitRange default request for a container.
	// +optional
	DefaultRequest corev1.ResourceList `json:"defaultRequest,omitempty"`

	// Default is the LimitRange default limit for a container.
	// +optional
	Default corev1.ResourceList `json:"default,omitempty"`
}

// LakeWorkspaceOverrideSpec selects settings that vary per workspace without extending lake's
// fixed workspaces table contract.
type LakeWorkspaceOverrideSpec struct {
	// Plan selects one entry in spec.workspaces.plans, or the built-in "default" plan.
	// +optional
	Plan string `json:"plan,omitempty"`

	// InternetEgress permits outbound TCP 443 to public addresses. Private, link-local, metadata,
	// node and cluster ranges remain denied.
	// +kubebuilder:default=false
	// +optional
	InternetEgress bool `json:"internetEgress,omitempty"`

	// Mesh enrols the workspace's pods in zetta-tunnel's mesh through its NetworkSandbox
	// (docs/infrastructure/mesh.md). Every path between them must then be a declared Connection.
	// +kubebuilder:default=false
	// +optional
	Mesh bool `json:"mesh,omitempty"`
}

// LakeTLSSpec configures HTTPS termination for a Lake Ingress.
type LakeTLSSpec struct {
	// SecretName names a kubernetes.io/tls Secret in spec.namespace for Host and ApexHost. Empty
	// uses the certificates the operator issues through DNSSecretRef.
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// LakeDeploySyncSpec configures deployment-row polling for a Lake.
type LakeDeploySyncSpec struct {
	// Interval is the deployment polling interval. Empty defaults to 15 seconds.
	// +kubebuilder:default="15s"
	// +optional
	Interval metav1.Duration `json:"interval,omitempty"`
}

// LakeStoreSpec describes the persistent libliquid store root for a Lake.
// LakeRolloutSpec controls pod replacement on a pod-template change.
type LakeRolloutSpec struct {
	// OverlapOnSharedStore lets a single-replica Lake whose store claim is ReadWriteOnce (or
	// missing) start its replacement pod beside the running one: RollingUpdate with maxSurge 1 and
	// maxUnavailable 0, and required pod affinity on kubernetes.io/hostname so the new pod mounts
	// the volume already attached to that node. For the overlap, two lake processes open the same
	// store root. Off by default, which keeps Recreate and its restart gap, until running two
	// processes on one store is proven safe. ReadWriteMany stores always surge; ReadWriteOncePod
	// stores always recreate, since a second pod can never mount them.
	// +optional
	OverlapOnSharedStore bool `json:"overlapOnSharedStore,omitempty"`
}

type LakeStoreSpec struct {
	// Size requested for an operator-created claim. It is used only when the claim is first
	// created; later changes do not replace or resize the data volume.
	// +kubebuilder:default="100Gi"
	// +optional
	Size resource.Quantity `json:"size,omitempty"`

	// StorageClassName for an operator-created claim. Empty uses the cluster default.
	// It is used only when the claim is first created.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// AccessModes for an operator-created claim. The default is ReadWriteMany because every
	// Deployment replica mounts the same store root. ReadWriteOnce requires spec.replicas=1.
	// It is used only when the claim is first created.
	// +kubebuilder:default={ReadWriteMany}
	// +kubebuilder:validation:MinItems=1
	// +optional
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`

	// ExistingClaim names a PersistentVolumeClaim in spec.namespace to mount instead of creating
	// lake-<name>-store. The operator does not adopt, modify, or delete this claim.
	// +optional
	ExistingClaim string `json:"existingClaim,omitempty"`
}

// LakeAgentsSpec configures lake's agents (docs/lake/agents.md).
type LakeAgentsSpec struct {
	// RuntimeImage is oramasearch/agent-runtime:<tag>, the one image every user-created Python
	// agent runs on. Empty uses the operator's default (internal/controller/lake_defaults.go).
	// +optional
	RuntimeImage string `json:"runtimeImage,omitempty"`
	// Library overrides the images of the specialized agents. Every Lake runs all nine agents of the
	// Agent Library on the operator's default images; an entry here replaces the image of its key
	// and cannot remove an agent. lake learns their addresses through LAKE_AGENT_ENDPOINTS.
	// +optional
	Library []LakeLibraryAgent `json:"library,omitempty"`
	// OpenAISecretName names a Secret in spec.namespace holding OPENAI_API_KEY, the general Orama
	// key. Only the library agents in spec.namespace get it; it is never copied into a workspace
	// namespace or given to a launch or schedule pod (orama-operator#779). People provide it; the
	// operator never creates or reads its value.
	// +optional
	OpenAISecretName string `json:"openaiSecretName,omitempty"`
	// Model is the frontier model agents default to.
	// +kubebuilder:default=gpt-6.1-sol
	// +optional
	Model string `json:"model,omitempty"`
	// JobDeadlineSeconds bounds one job launch.
	// +kubebuilder:validation:Minimum=60
	// +optional
	JobDeadlineSeconds *int64 `json:"jobDeadlineSeconds,omitempty"`
}

// LakeLibraryAgent is one specialized agent of lake's Agent Library.
type LakeLibraryAgent struct {
	// Key is the agent's repository name, which lake's Agent Library uses too.
	// +kubebuilder:validation:Enum=agent-ask-orama;agent-project;agent-autosuggest;agent-creator;agent-eval-generator;agent-autosuggest-corpus;agent-project-corpus;agent-application;agent-experiments
	Key string `json:"key"`
	// Image is the agent's private image and tag.
	Image string `json:"image"`
}

// LakeSealingSpec selects the desired cluster sealing-key generation.
type LakeSealingSpec struct {
	// Generation rotates the sealing key when increased. It cannot be decreased.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Generation int64 `json:"generation,omitempty"`
}

// LakePostgresSpec describes the control-plane Postgres for a Lake.
type LakePostgresSpec struct {
	// Image selects the CNPG-compatible Postgres image. Empty keeps CNPG's default. Restore and
	// upgrade rehearsals require the running source image to be pinned by SHA-256 digest.
	// +kubebuilder:validation:Pattern=`^.+@sha256:[a-f0-9]{64}$`
	// +optional
	Image string `json:"image,omitempty"`

	// Enabled provisions the CNPG Cluster. Default true. Setting it false tears the cluster
	// down (disable is not "leave running"), which deletes its data.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Instances is the CNPG instance count (1 = single primary; 2+ adds streaming replicas).
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Instances *int32 `json:"instances,omitempty"`

	// StorageSize is the per-instance PVC size. Default 50Gi: a Lake is a production control plane,
	// and CNPG grows a PVC in place when the StorageClass allows expansion, so start with headroom.
	// +kubebuilder:default="50Gi"
	// +optional
	StorageSize string `json:"storageSize,omitempty"`

	// StorageClassName for the PVCs; empty = the cluster default.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// Backup configures scheduled Barman object-store backups of the control-plane Cluster.
	// +optional
	Backup LakePostgresBackupSpec `json:"backup,omitempty"`
}

// LakeZettaSpec describes the owned Zetta data plane for a Lake.
type LakeZettaSpec struct {
	// Enabled provisions the OramaZettaCluster. Default true. Setting it false tears the cluster
	// down, including its per-node data volumes.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Image is the Zetta image. Empty uses the operator's default (internal/controller/lake_defaults.go).
	// +optional
	Image string `json:"image,omitempty"`

	// Replicas is the number of equal-peer Zetta nodes.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Storage configures each Zetta node's persistent data volume.
	// +optional
	Storage OramaZettaStorage `json:"storage,omitempty"`

	// Env is copied to the owned OramaZettaCluster spec.env, followed by the operator's default Zetta
	// env for every name it does not set (ZETTA_MALLOC_TRIM_THRESHOLD_BYTES=67108864,
	// MALLOC_ARENA_MAX=2, and an empty ZETTA_EGRESS_ALLOW, which denies all egress as the Lake
	// namespace's NetworkSandbox does). Operator-managed Zetta environment names remain reserved and
	// are reported by the child cluster when ignored.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Reachability declares whether this data-plane slice is dial-out-only or wan-accessible.
	// Empty defaults to dial-out-only.
	// +kubebuilder:validation:Enum=dial-out-only;wan-accessible
	// +optional
	Reachability string `json:"reachability,omitempty"`
}

// LakePostgresBackupSpec describes scheduled object-store backups for a Lake control-plane
// Postgres cluster. The Secret is in spec.namespace and has the same R2/S3 shape as
// OramaCloudV1's r2SecretRef: s3ApiEndpoint, accessKeyId, and secretAccessKey.
type LakePostgresBackupSpec struct {
	// Enabled turns on the CNPG Barman object store and ScheduledBackup. Default false.
	// +kubebuilder:default=false
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Schedule is a six-field CNPG cron expression.
	// +kubebuilder:default="0 0 3 * * *"
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// Retention is the Barman retentionPolicy.
	// +kubebuilder:default="30d"
	// +optional
	Retention string `json:"retention,omitempty"`

	// Destination identifies the object-store location. Bucket is required when Enabled is true.
	// +optional
	Destination LakePostgresBackupDestination `json:"destination,omitempty"`

	// SecretRef names the R2/S3 credentials Secret in spec.namespace. It is required when
	// Enabled is true and must contain s3ApiEndpoint, accessKeyId, and secretAccessKey.
	// +optional
	SecretRef corev1.LocalObjectReference `json:"secretRef,omitempty"`
}

// LakePostgresBackupDestination identifies the Barman object-store location.
type LakePostgresBackupDestination struct {
	// Bucket is the object-store bucket name. Required when backups are enabled.
	// +optional
	Bucket string `json:"bucket,omitempty"`

	// Path is the bucket prefix. Empty defaults to lake-<name>/controldb.
	// +optional
	Path string `json:"path,omitempty"`

	// EndpointURL overrides the Secret's s3ApiEndpoint.
	// +optional
	EndpointURL string `json:"endpointURL,omitempty"`
}

// LakeStatus defines the observed state of Lake.
type LakeStatus struct {
	// ReadyReplicas mirrors the Deployment's ready replicas.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// PostgresReady is true once the control-plane CNPG Cluster has at least one ready instance.
	// +optional
	PostgresReady bool `json:"postgresReady,omitempty"`
	// PostgresInstances is ready/desired for the CNPG Cluster, e.g. "1/1"; empty when disabled.
	// +optional
	PostgresInstances string `json:"postgresInstances,omitempty"`
	// ZettaReady is true once every desired node in the owned data-plane cluster is ready.
	// +optional
	ZettaReady bool `json:"zettaReady,omitempty"`
	// SchemaPhase mirrors the owned PostgresSchema: Pending | Applying | Applied | Failed.
	// +optional
	SchemaPhase string `json:"schemaPhase,omitempty"`
	// SchemaVersion is the applied schema version, from the PostgresSchema: the vendored schema's
	// content hash (sha256:<12 hex>), or image:<digest> when the image migrates itself
	// (orama-operator#936).
	// +optional
	SchemaVersion string `json:"schemaVersion,omitempty"`
	// ImageManifest is what the Lake's image reported through `lake manifest`, read once per image
	// by a Job (orama-operator#936): its schema, the Stripe events it handles and its Stripe
	// products. Supported is false for an image that predates the command; the operator then uses
	// its vendored schema and lists.
	// +optional
	ImageManifest *LakeImageManifest `json:"imageManifest,omitempty"`
	// BackupReady is true when the ScheduledBackup exists and the newest CNPG Backup completed.
	// +optional
	BackupReady bool `json:"backupReady,omitempty"`
	// LastBackupTime is the completion time reported by the newest CNPG Backup.
	// +optional
	LastBackupTime *metav1.Time `json:"lastBackupTime,omitempty"`
	// DataServices reports the operational evidence for every data service associated with the
	// Lake. Managed is true only when every applicable procedure has a successful rehearsal.
	// +listType=map
	// +listMapKey=name
	// +optional
	DataServices []LakeDataServiceStatus `json:"dataServices,omitempty"`
	// DataServicesSummary is the managed/unmanaged count displayed by kubectl get lakes.
	// +optional
	DataServicesSummary string `json:"dataServicesSummary,omitempty"`
	// Store reports the selected claim and its observed binding state.
	// +optional
	Store *LakeStoreStatus `json:"store,omitempty"`
	// Sealing publishes the cluster-bound recipient used by authoring tools and lake.
	// +optional
	Sealing *LakeSealingStatus `json:"sealing,omitempty"`
	// CustomDomain reports spec.customDomain: the records the customer creates, whether they are
	// found, and whether the host is routed (docs/lake/custom-domains.md). Absent without it.
	// +optional
	CustomDomain *LakeCustomDomainStatus `json:"customDomain,omitempty"`
	// Conditions includes StoreReady, BackupReady, ZettaReady, and FeaturesReady, including invalid
	// configuration details.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Phase is a coarse summary in the operator's vocabulary: Progressing, Ready, Degraded.
	// +optional
	Phase string `json:"phase,omitempty"`
}

// LakeImageManifest is the part of a lake image's `lake manifest` the operator acts on.
type LakeImageManifest struct {
	// Image is the spec.image the manifest was read from.
	Image string `json:"image"`
	// ImageRef is the image by digest, as the manifest pod ran it (from its imageID); the migration
	// Job runs this reference, so it runs the bytes the manifest came from. Empty when the runtime
	// did not report one.
	// +optional
	ImageRef string `json:"imageRef,omitempty"`
	// Supported is false when the image predates `lake manifest` (it exits 2 with its usage text
	// and no JSON): the operator then applies its vendored schema and uses its own Stripe lists.
	Supported bool `json:"supported"`
	// Version and Revision are the lake release and source commit the image reports.
	// +optional
	Version string `json:"version,omitempty"`
	// +optional
	Revision string `json:"revision,omitempty"`
	// SchemaVersion is the embedded schema's sha256:<12 hex>.
	// +optional
	SchemaVersion string `json:"schemaVersion,omitempty"`
	// LatestMigration is the newest migration the embedded schema records.
	// +optional
	LatestMigration string `json:"latestMigration,omitempty"`
	// GuardTable is the guard table the newest migration creates.
	// +optional
	GuardTable string `json:"guardTable,omitempty"`
	// GuardTables are every *_guard table the schema creates: what a restore rehearsal counts.
	// +optional
	GuardTables []string `json:"guardTables,omitempty"`
	// Migrations are every migration the schema records: what a restore rehearsal requires.
	// +optional
	Migrations []string `json:"migrations,omitempty"`
	// MigrateArgs are the migration command's arguments, wait flag included.
	// +optional
	MigrateArgs []string `json:"migrateArgs,omitempty"`
	// DatabaseEnv is the variable the migration command reads the control database URI from.
	// +optional
	DatabaseEnv string `json:"databaseEnv,omitempty"`
	// StripeWebhookEvents are the Stripe events the image handles: the Lake's webhook endpoint
	// subscribes to exactly these.
	// +optional
	StripeWebhookEvents []string `json:"stripeWebhookEvents,omitempty"`
	// StripeProducts are the products the image bills under: the Lake's PaymentCatalog.
	// +optional
	StripeProducts []LakeStripeProduct `json:"stripeProducts,omitempty"`
	// ServiceConnectionsOnDemand is true when the image's manifest environment lists
	// LAKE_SERVICE_CONNECTION_URL: lake then asks the operator for one service's connection when it
	// needs it, and the operator stops mounting every service credential into the Lake pod
	// (operator#773, docs/lake/services.md#connections-on-demand).
	// +optional
	ServiceConnectionsOnDemand bool `json:"serviceConnectionsOnDemand,omitempty"`
	// IdentityPassphraseFile is true when the image's manifest environment lists
	// LIQUID_IDENTITY_PASSPHRASE_FILE: liquid then wraps a new identity's key under a passphrase, and
	// the operator gives the Lake pod an owner-only passphrase file (operator#1168,
	// docs/lake/README.md#identity-passphrase).
	// +optional
	IdentityPassphraseFile bool `json:"identityPassphraseFile,omitempty"`
}

// LakeStripeProduct is one product a lake image bills under.
type LakeStripeProduct struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// LakeDataServiceStatus reports whether a Lake data service has proven lifecycle procedures.
type LakeDataServiceStatus struct {
	// Name is the stable service identifier within the Lake.
	Name string `json:"name"`
	// Kind identifies the service implementation.
	// +kubebuilder:validation:Enum=postgres;zetta
	Kind string `json:"kind"`
	// Managed is true only when every procedure required by the current configuration is tested.
	Managed bool `json:"managed"`
	// Backup is tested, untested, or none.
	// +kubebuilder:validation:Enum=tested;untested;none
	Backup string `json:"backup"`
	// Restore is tested, untested, or none.
	// +kubebuilder:validation:Enum=tested;untested;none
	Restore string `json:"restore"`
	// Upgrade is tested, untested, or none.
	// +kubebuilder:validation:Enum=tested;untested;none
	Upgrade string `json:"upgrade"`
	// Failover is tested, untested, or none.
	// +kubebuilder:validation:Enum=tested;untested;none
	Failover string `json:"failover"`
	// LastVerified is the completion time of the newest rehearsal for this service.
	// +optional
	LastVerified *metav1.Time `json:"lastVerified,omitempty"`
	// Notes explains an unmanaged label, an unavailable procedure, or the last failed rehearsal.
	// +optional
	Notes string `json:"notes,omitempty"`
}

// LakeSealingStatus publishes the active recipient and retained-key progress.
type LakeSealingStatus struct {
	// Cluster is the AAD cluster value. It is derived from the Lake UID.
	Cluster string `json:"cluster"`
	// Generation is the active key generation.
	Generation int64 `json:"generation"`
	// KID identifies the active public key.
	KID string `json:"kid"`
	// PublicKey is the raw 32-byte X25519 public key encoded as unpadded base64url.
	PublicKey string `json:"publicKey"`
	// PreviousKeys reports retained private keys and records that still require each one.
	// +optional
	PreviousKeys []LakeSealingPreviousKeyStatus `json:"previousKeys,omitempty"`
	// UnresolvedRecords counts malformed records whose KID cannot be established. Old keys are
	// retained while this is nonzero.
	// +optional
	UnresolvedRecords int32 `json:"unresolvedRecords,omitempty"`
}

// LakeSealingPreviousKeyStatus reports why an old private key remains available.
type LakeSealingPreviousKeyStatus struct {
	// KID identifies the retained key.
	KID string `json:"kid"`
	// Remaining is the number of sealed records that still name this key.
	Remaining int32 `json:"remaining"`
}

// LakeStoreStatus is the observed state of the claim mounted at LAKE_STORE_ROOT.
type LakeStoreStatus struct {
	// ClaimName is the selected existing or operator-created PersistentVolumeClaim.
	// +optional
	ClaimName string `json:"claimName,omitempty"`

	// Phase is the PersistentVolumeClaim phase, such as Pending, Bound, or Lost.
	// +optional
	Phase corev1.PersistentVolumeClaimPhase `json:"phase,omitempty"`

	// Capacity is the bound storage capacity reported by the claim.
	// +optional
	Capacity *resource.Quantity `json:"capacity,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.namespace`
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.host`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.imageManifest.version`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.status.imageManifest.revision`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="DB",type=string,JSONPath=`.status.postgresInstances`
// +kubebuilder:printcolumn:name="Zetta",type=boolean,JSONPath=`.status.zettaReady`
// +kubebuilder:printcolumn:name="Schema",type=string,JSONPath=`.status.schemaPhase`
// +kubebuilder:printcolumn:name="Store",type=string,JSONPath=`.status.store.claimName`
// +kubebuilder:printcolumn:name="Backup",type=date,JSONPath=`.status.lastBackupTime`
// +kubebuilder:printcolumn:name="Data",type=string,JSONPath=`.status.dataServicesSummary`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// Lake is the Schema for the lakes API
type Lake struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Lake
	// +required
	Spec LakeSpec `json:"spec"`

	// status defines the observed state of Lake
	// +optional
	Status LakeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// LakeList contains a list of Lake
type LakeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Lake `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Lake{}, &LakeList{})
}

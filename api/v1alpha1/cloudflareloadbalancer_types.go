/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CloudflareLoadBalancerSpec is the desired state of a Cloudflare **global Load Balancer** — a
// single hostname served across the cluster's origins with health checks, geo/proximity steering,
// and failover. The API token, zone, and account all come from the referenced Secret (keys
// `token`+`zoneID`+`accountID`), so no credential lives in spec. This models three Cloudflare
// Load Balancing objects together: an account-scoped **Monitor**, one or more account-scoped
// **Pools**, and a zone-scoped **Load Balancer**.
type CloudflareLoadBalancerSpec struct {
	// SecretRef names the Secret holding the Cloudflare credentials (`token`, `zoneID`,
	// `accountID`) — normally the CR's `spec.cloudflare.apiTokenSecretRef`
	// (the `cloudflare-api-token` Secret).
	SecretRef string `json:"secretRef"`

	// Hostname is the FQDN the load balancer answers on (e.g. zetta.example.net). Cloudflare
	// creates/manages the DNS entry for this name in the zone; do NOT also manage it with a
	// CloudflareDNSRecord — the LB owns the hostname.
	Hostname string `json:"hostname"`

	// Pools are the origin pools (one or more). Under normal operation traffic is spread across
	// the healthy pools per SteeringPolicy; on total failure it drops to FallbackPool. The pools
	// become the load balancer's `default_pools` (in declared order).
	// +kubebuilder:validation:MinItems=1
	Pools []OriginPool `json:"pools"`

	// Monitor is the health monitor attached to every pool. Cloudflare probes each origin on this
	// schedule and drops unhealthy origins from rotation. (Issue #74 auto-populates pool origins
	// from the live wan-accessible node set; this CRD owns the LB/pool/monitor resources.)
	// +optional
	Monitor HealthMonitor `json:"monitor,omitempty"`

	// SteeringPolicy selects how Cloudflare routes across the pools:
	//   off        — fixed failover order (first healthy pool wins).
	//   geo        — route by client region using RegionPools (nearest configured region).
	//   dynamic    — route by measured pool latency (maps to the API's `dynamic_latency`).
	//   proximity  — route to the pool/origin geographically nearest the client.
	//   random     — spread randomly across healthy pools (weighted by origin weight).
	// +kubebuilder:validation:Enum=off;geo;dynamic;proximity;random
	// +kubebuilder:default=geo
	SteeringPolicy string `json:"steeringPolicy,omitempty"`

	// FallbackPool names the pool (by its Pools[].Name) used as the last resort when ALL default
	// pools are unhealthy. Cloudflare ignores the fallback pool's own health. Empty = the last
	// declared pool is used (Cloudflare's default behaviour).
	// +optional
	FallbackPool string `json:"fallbackPool,omitempty"`

	// RegionPools maps a Cloudflare region code (e.g. WNAM, ENAM, WEUR, SEAS) to an ordered list
	// of pool names (Pools[].Name), for `geo` steering. Optional; only meaningful when
	// SteeringPolicy=geo. Without it, `geo` falls back to the default-pool failover order.
	// +optional
	RegionPools map[string][]string `json:"regionPools,omitempty"`

	// Proxied routes the load-balanced hostname through Cloudflare's proxy (orange-cloud).
	// +kubebuilder:default=true
	Proxied bool `json:"proxied,omitempty"`

	// SessionAffinity pins a client to the same origin: none | cookie | ip_cookie | header.
	// +kubebuilder:validation:Enum=none;cookie;ip_cookie;header
	// +kubebuilder:default=none
	// +optional
	SessionAffinity string `json:"sessionAffinity,omitempty"`
}

// OriginPool is one Cloudflare origin pool — a named group of origins that Cloudflare treats as a
// single failover/steering unit.
type OriginPool struct {
	// Name identifies the pool within this CR. The Cloudflare pool is named
	// `<cr-name>-<Name>` (namespaced to avoid collisions across CRs in the account).
	Name string `json:"name"`

	// Origins are the pool's backend endpoints (the cluster's wan-accessible nodes).
	// +kubebuilder:validation:MinItems=1
	Origins []LoadBalancerOrigin `json:"origins"`

	// MinimumOrigins is how many origins must be healthy for the pool itself to be healthy
	// (Cloudflare `minimum_origins`). Defaults to 1.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinimumOrigins int `json:"minimumOrigins,omitempty"`

	// Enabled toggles the whole pool. Defaults true.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// UseMonitor attaches spec.Monitor to this pool (the "optional health-monitor ref"). Defaults
	// true — the point of a pool is health-checked origins. Set false for an unmonitored pool.
	// +kubebuilder:default=true
	// +optional
	UseMonitor bool `json:"useMonitor,omitempty"`
}

// LoadBalancerOrigin is one backend endpoint inside a pool.
type LoadBalancerOrigin struct {
	// Name uniquely identifies the origin within its pool.
	Name string `json:"name"`

	// Address is the origin's IP or hostname (e.g. a node's public endpoint). For proxied LBs an
	// FQDN is resolved by Cloudflare; the health monitor probes this address on the monitor port.
	Address string `json:"address"`

	// Enabled toggles whether this origin receives traffic. Defaults true.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Weight biases weighted steering within the pool — a decimal in [0,1] as a string (Cloudflare
	// stores origin weight as a float). Defaults "1" (equal weighting). Out-of-range or
	// unparseable values are clamped into [0,1].
	// +kubebuilder:default="1"
	// +optional
	Weight string `json:"weight,omitempty"`
}

// HealthMonitor configures the Cloudflare Load Balancer Monitor that probes each origin.
type HealthMonitor struct {
	// Type is the probe protocol: http | https | tcp. Defaults https.
	// +kubebuilder:validation:Enum=http;https;tcp
	// +kubebuilder:default=https
	// +optional
	Type string `json:"type,omitempty"`

	// Method is the HTTP method for http/https probes. Defaults GET.
	// +kubebuilder:default=GET
	// +optional
	Method string `json:"method,omitempty"`

	// Path is the probe path for http/https, e.g. /v1/server_info. Defaults /v1/server_info.
	// +kubebuilder:default=/v1/server_info
	// +optional
	Path string `json:"path,omitempty"`

	// Port overrides the probe port (0 = the origin's default for the scheme).
	// +optional
	Port int `json:"port,omitempty"`

	// ExpectedCodes is the healthy HTTP status (exact "200" or a class like "2xx"). Defaults "200".
	// +kubebuilder:default="200"
	// +optional
	ExpectedCodes string `json:"expectedCodes,omitempty"`

	// ExpectedBody, when set, must appear in the response body for the origin to be healthy.
	// +optional
	ExpectedBody string `json:"expectedBody,omitempty"`

	// Interval is seconds between probes (plan minimums apply). Defaults 60.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=1
	// +optional
	Interval int `json:"interval,omitempty"`

	// Timeout is seconds to wait for a probe response before it fails. Defaults 5.
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +optional
	Timeout int `json:"timeout,omitempty"`

	// Retries is the number of retries before an origin is marked unhealthy. Defaults 2.
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=0
	// +optional
	Retries int `json:"retries,omitempty"`

	// FollowRedirects follows 3xx during http/https probes. Defaults false.
	// +optional
	FollowRedirects bool `json:"followRedirects,omitempty"`
}

// CloudflareLoadBalancerStatus is the observed state — the provisioned Cloudflare objects and
// their health. Written to the status subresource ONLY on a real state transition (anti-flap,
// principle #7): a steady load balancer produces zero control-plane traffic.
type CloudflareLoadBalancerStatus struct {
	// LoadBalancerID is the zone-scoped Cloudflare Load Balancer ID (set once created).
	// +optional
	LoadBalancerID string `json:"loadBalancerID,omitempty"`

	// MonitorID is the account-scoped Cloudflare Monitor ID (set once created).
	// +optional
	MonitorID string `json:"monitorID,omitempty"`

	// Pools reflects each managed pool — its Cloudflare ID and last-observed health.
	// +optional
	// +listType=map
	// +listMapKey=name
	Pools []LoadBalancerPoolStatus `json:"pools,omitempty"`

	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Message is a human-readable summary of the current state (or the last error).
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions carry the standard `Ready` condition (anti-flap). Reason distinguishes
	// WaitingForCredentials, LoadBalancingNotEnabled, ReconcileError, and Reconciled.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// LoadBalancerPoolStatus is the observed state of one managed pool.
type LoadBalancerPoolStatus struct {
	// Name is the pool's spec name (Pools[].Name).
	Name string `json:"name"`
	// ID is the Cloudflare pool ID.
	// +optional
	ID string `json:"id,omitempty"`
	// Health is the last-observed pool health: healthy | unhealthy | unknown.
	// +optional
	Health string `json:"health,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=cflb
//+kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=`.spec.hostname`
//+kubebuilder:printcolumn:name="Steering",type=string,JSONPath=`.spec.steeringPolicy`
//+kubebuilder:printcolumn:name="LB",type=string,JSONPath=`.status.loadBalancerID`
//+kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`

// CloudflareLoadBalancer is a Cloudflare global Load Balancer the operator provisions via the CF
// Load Balancing API, owned by an OramaCloudV1 CR. It reconciles a Monitor, one or more Pools, and
// the zone Load Balancer; a finalizer tears all three down (LB → pools → monitor) on delete, so
// removing the CR cascades to real external cleanup. Cloudflare Load Balancing is a PAID add-on:
// until it is enabled on the account the reconciler is a safe no-op that surfaces a clear
// `Ready=False (LoadBalancingNotEnabled)` condition rather than crash-looping.
type CloudflareLoadBalancer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CloudflareLoadBalancerSpec   `json:"spec,omitempty"`
	Status CloudflareLoadBalancerStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// CloudflareLoadBalancerList contains a list of CloudflareLoadBalancer.
type CloudflareLoadBalancerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudflareLoadBalancer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CloudflareLoadBalancer{}, &CloudflareLoadBalancerList{})
}

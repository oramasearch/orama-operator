/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// cloudflareOriginPullCA is the CA of the client certificate Cloudflare presents to origins when a
// zone has authenticated origin pulls on (the zone setting tls_client_auth, "global AOP"). It is
// Cloudflare's published file, unchanged:
// https://developers.cloudflare.com/ssl/static/authenticated_origin_pull_ca.pem
// (CN origin-pull.cloudflare.net, valid until 2029-11-01, SHA-256 fingerprint
// 9A:1A:C2:B4:BE:15:F9:F2:7E:EE:20:A7:34:CB:A4:E9:89:8F:61:00:1B:3B:D7:C8:4B:69:B5:6A:3E:25:A2:B9).
//
//go:embed assets/cloudflare-origin-pull-ca.pem
var cloudflareOriginPullCA string

// Origin encryption modes, weakest first (Cloudflare's `ssl` zone setting values).
const (
	edgeOriginOff      = "off"
	edgeOriginFlexible = "flexible"
	edgeOriginFull     = "full"
	edgeOriginStrict   = "strict"
)

// edgeOriginRank orders the modes; a mode the operator does not know ranks lowest, so it is never
// treated as stronger than it is.
func edgeOriginRank(mode string) int {
	switch mode {
	case edgeOriginFlexible:
		return 1
	case edgeOriginFull:
		return 2
	case edgeOriginStrict:
		return 3
	}
	return 0
}

// edgeOriginEncrypted is whether the edge reaches origins over TLS, so an origin may redirect plain
// HTTP to HTTPS and may require the edge's client certificate.
func edgeOriginEncrypted(mode string) bool { return edgeOriginRank(mode) >= 2 }

const (
	edgePhaseReady       = "Ready"
	edgePhaseProgressing = "Progressing"
	edgePhaseFailed      = "Failed"

	edgeConditionCredentials        = "Credentials"
	edgeConditionOriginCertificates = "OriginCertificates"
	edgeConditionEdgeCertificates   = "EdgeCertificates"
	edgeConditionOriginTLSStrict    = "OriginTLSStrict"
	edgeConditionOriginPulls        = "AuthenticatedOriginPulls"
	edgeConditionAlwaysUseHTTPS     = "AlwaysUseHTTPS"
	edgeConditionZoneName           = "ZoneName"

	// edgeZoneRecheck is how often the zone is read and its hosts probed. A probe is a TLS
	// handshake per proxied host; status is written only when something changes.
	edgeZoneRecheck = 10 * time.Minute
	// edgeZoneHostLimit bounds status.hosts.
	edgeZoneHostLimit = 50
	// edgeProbeLabel is the name a wildcard record is probed with: *.eu.lake.live is checked as
	// orama-edge-probe.eu.lake.live.
	edgeProbeLabel = "orama-edge-probe"
)

// The permission each Cloudflare call needs, as the dashboard names it, for the message a 401 or
// 403 produces.
const (
	cfPermissionSettingsRead = "Zone > Zone Settings > Read (Edit includes it)"
	cfPermissionSettingsEdit = "Zone > Zone Settings > Edit"
	cfPermissionDNSRead      = "Zone > DNS > Read (Edit includes it)"
	// The SSL and Certificates permissions cover the edge certificate orders
	// (cloudflare_edge_certificates.go) and zone-level origin pulls (cloudflarezonepolicy_originpull.go).
	cfPermissionCertificatesRead = "Zone > SSL and Certificates > Read (Edit includes it)"
	cfPermissionCertificatesEdit = "Zone > SSL and Certificates > Edit"
	// cfPermissionZoneRead ("Zone > Zone > Read") is declared with the host policy's permissions.
)

// edgeProber checks hosts with TLS handshakes. Tests replace it.
type edgeProber interface {
	// Origin dials address:443 with SNI serverName and returns nil when the origin presents a
	// certificate valid for serverName. handshake is true when a TLS handshake completed at all.
	Origin(ctx context.Context, address, serverName string) (handshake bool, err error)
	// Edge resolves serverName, dials it on 443 and returns nil when the edge presents a valid
	// certificate for it.
	Edge(ctx context.Context, serverName string) error
	// ClientCAs dials address:443 with SNI serverName and reports whether the origin asks for a
	// client certificate and, if so, the subjects (DER) of the CAs it accepts.
	ClientCAs(ctx context.Context, address, serverName string) (requested bool, acceptable [][]byte, err error)
}

// tlsEdgeProber is the real prober: Go's TLS client with the system roots (the operator image is
// distroless/static, which carries them). It sends no client certificate, so an origin that
// requires one still completes the handshake and answers 400; the certificate is what is checked.
type tlsEdgeProber struct{}

func (tlsEdgeProber) Origin(ctx context.Context, address, serverName string) (bool, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	// First the real check: a verified handshake.
	conn, err := (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}}).
		DialContext(ctx, "tcp", net.JoinHostPort(address, "443"))
	if err == nil {
		_ = conn.Close()
		return true, nil
	}
	verifyErr := err
	// Then whether TLS works at all, for the Full step. Only the handshake is made; nothing is sent.
	insecure := &tls.Config{ServerName: serverName, InsecureSkipVerify: true} //nolint:gosec // learns only whether the origin speaks TLS
	conn, err = (&tls.Dialer{NetDialer: dialer, Config: insecure}).DialContext(ctx, "tcp", net.JoinHostPort(address, "443"))
	if err != nil {
		return false, verifyErr
	}
	_ = conn.Close()
	return true, verifyErr
}

func (tlsEdgeProber) ClientCAs(ctx context.Context, address, serverName string) (bool, [][]byte, error) {
	return probeClientCAs(ctx, net.JoinHostPort(address, "443"), serverName)
}

// probeClientCAs makes a TLS handshake with hostPort and returns the origin's CertificateRequest.
func probeClientCAs(ctx context.Context, hostPort, serverName string) (bool, [][]byte, error) {
	var requested bool
	var acceptable [][]byte
	config := &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
		// Only the server's CertificateRequest is read; the origin's own certificate is checked by
		// Origin.
		InsecureSkipVerify: true, //nolint:gosec // learns only which client CAs the origin accepts
		GetClientCertificate: func(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			requested = true
			acceptable = append([][]byte(nil), info.AcceptableCAs...)
			return &tls.Certificate{}, nil
		},
	}
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: config}
	conn, err := dialer.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		if requested {
			// The origin refused the empty certificate after asking for one; what it asked is known.
			return true, acceptable, nil
		}
		return false, nil, err
	}
	_ = conn.Close()
	return requested, acceptable, nil
}

func (tlsEdgeProber) Edge(ctx context.Context, serverName string) error {
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(serverName, "443"))
	if err != nil {
		return err
	}
	return conn.Close()
}

// CloudflareZonePolicyReconciler applies an EdgeZonePolicy to a Cloudflare zone: it reads the
// zone's settings and proxied DNS records, probes every proxied host, and turns each setting on only
// when its check passes (EdgeZonePolicySpec). It never turns a setting off.
type CloudflareZonePolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIBase is the Cloudflare API base URL; empty means the public API. Tests point it at a fake.
	APIBase string
	// Prober checks hosts; nil uses TLS handshakes.
	Prober edgeProber
	// OperatorNamespace holds each zone's origin-pull CA and client certificate Secret
	// (orama-edge-origin-pull-<zone>) for the policies that name no distributed one
	// (spec.originPullSecretRef); on the managing cluster it is the Secret the LakeSets copy. Empty
	// uses the policy's namespace.
	OperatorNamespace string
	// Now is the clock for certificate lifetimes; nil uses time.Now.
	Now func() time.Time
}

func (r *CloudflareZonePolicyReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *CloudflareZonePolicyReconciler) api() string {
	if r.APIBase != "" {
		return r.APIBase
	}
	return cfAPIBase
}

func (r *CloudflareZonePolicyReconciler) prober() edgeProber {
	if r.Prober != nil {
		return r.Prober
	}
	return tlsEdgeProber{}
}

// +kubebuilder:rbac:groups=orama.cloud,resources=cloudflarezonepolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=cloudflarezonepolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *CloudflareZonePolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var policy oramav1alpha1.CloudflareZonePolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	previous := *policy.Status.DeepCopy()
	status := r.observe(ctx, &policy, previous)
	status.ObservedGeneration = policy.Generation
	status.Conditions = mergeEdgeConditions(previous.Conditions, status.Conditions, policy.Generation)
	if !equality.Semantic.DeepEqual(previous, status) {
		policy.Status = status
		if err := r.Status().Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: edgeZoneRecheck}, nil
}

// cfZoneSetting is one zone setting as the API returns it.
type cfZoneSetting struct {
	ID    string `json:"id"`
	Value any    `json:"value"`
}

type cfZoneSettingResponse struct {
	Result cfZoneSetting `json:"result"`
}

func (s cfZoneSetting) str() string {
	if v, ok := s.Value.(string); ok {
		return v
	}
	return ""
}

func (r *CloudflareZonePolicyReconciler) setting(ctx context.Context, token, zoneID, name string) (string, error) {
	var out cfZoneSettingResponse
	if err := cfDo(ctx, token, http.MethodGet, fmt.Sprintf("%s/zones/%s/settings/%s", r.api(), zoneID, name), nil, &out); err != nil {
		return "", err
	}
	return out.Result.str(), nil
}

func (r *CloudflareZonePolicyReconciler) setSetting(ctx context.Context, token, zoneID, name, value string) error {
	return cfDo(ctx, token, http.MethodPatch, fmt.Sprintf("%s/zones/%s/settings/%s", r.api(), zoneID, name), map[string]string{"value": value}, nil)
}

type cfZoneResponse struct {
	Result struct {
		Name string `json:"name"`
	} `json:"result"`
}

// zoneName reads the zone's name (its apex domain). cert-manager's Cloudflare DNS-01 solver needs
// the same permission, Zone > Zone > Read, to find the zone of a name.
func (r *CloudflareZonePolicyReconciler) zoneName(ctx context.Context, token, zoneID string) (string, error) {
	var out cfZoneResponse
	if err := cfDo(ctx, token, http.MethodGet, fmt.Sprintf("%s/zones/%s", r.api(), zoneID), nil, &out); err != nil {
		return "", err
	}
	name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(out.Result.Name)), ".")
	if name == "" {
		return "", errors.New("the zone has no name")
	}
	return name, nil
}

type cfDNSRecordList struct {
	Result     []cfDNSRecord `json:"result"`
	ResultInfo struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

// proxiedRecords lists the zone's proxied A, AAAA and CNAME records.
func (r *CloudflareZonePolicyReconciler) proxiedRecords(ctx context.Context, token, zoneID string) ([]cfDNSRecord, error) {
	var records []cfDNSRecord
	for page := 1; page <= 50; page++ {
		var out cfDNSRecordList
		url := fmt.Sprintf("%s/zones/%s/dns_records?proxied=true&per_page=500&page=%d", r.api(), zoneID, page)
		if err := cfDo(ctx, token, http.MethodGet, url, nil, &out); err != nil {
			return nil, err
		}
		for _, record := range out.Result {
			if record.Proxied && (record.Type == "A" || record.Type == "AAAA" || record.Type == "CNAME") {
				records = append(records, record)
			}
		}
		if out.ResultInfo.TotalPages <= page {
			break
		}
	}
	return records, nil
}

// cfDenied is whether err is Cloudflare refusing the token.
func cfDenied(err error) bool {
	var apiErr *cfAPIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden)
}

// cfSettingAbsent is whether err is a 404, or the 400 Cloudflare answers for a setting the zone lacks.
func cfSettingAbsent(err error) bool {
	var apiErr *cfAPIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusBadRequest)
}

// cfErrorMessage names the permission a denied call needs, and otherwise the HTTP status only (a
// Cloudflare error body is not copied into status).
func cfErrorMessage(action string, err error, permission string) string {
	if cfDenied(err) {
		return fmt.Sprintf("the API token may not %s: add %s for this zone", action, permission)
	}
	var apiErr *cfAPIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("cannot %s: Cloudflare answered HTTP %d", action, apiErr.StatusCode)
	}
	return fmt.Sprintf("cannot %s: %v", action, err)
}

func (r *CloudflareZonePolicyReconciler) token(ctx context.Context, policy *oramav1alpha1.CloudflareZonePolicy) (string, string) {
	ref := policy.Spec.APITokenSecretRef
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: policy.Namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Sprintf("token Secret %q does not exist", ref.Name)
		}
		return "", fmt.Sprintf("read token Secret %q: %v", ref.Name, err)
	}
	token := strings.TrimSpace(string(secret.Data[ref.Key]))
	if token == "" {
		return "", fmt.Sprintf("token Secret %q has no key %q", ref.Name, ref.Key)
	}
	return token, ""
}

// failedEdgeStatus reports a zone that cannot be read. It keeps what was last observed (the
// operator never turns a setting off, so the last observation is still the best one), so a
// Cloudflare outage does not make the Lakes drop their redirect or client-certificate check.
func failedEdgeStatus(previous oramav1alpha1.EdgeZonePolicyStatus, reason, message string) oramav1alpha1.EdgeZonePolicyStatus {
	status := previous
	status.Phase = edgePhaseFailed
	status.Message = message
	if status.OriginClientCAPEM == "" {
		status.OriginClientCAPEM = cloudflareOriginPullCA
	}
	status.Conditions = []metav1.Condition{{Type: edgeConditionCredentials, Status: metav1.ConditionFalse, Reason: reason, Message: message}}
	for _, condition := range previous.Conditions {
		if condition.Type != edgeConditionCredentials {
			status.Conditions = append(status.Conditions, condition)
		}
	}
	return status
}

// observe reads the zone, applies what is safe, and returns the zone's status.
func (r *CloudflareZonePolicyReconciler) observe(ctx context.Context, policy *oramav1alpha1.CloudflareZonePolicy, previous oramav1alpha1.EdgeZonePolicyStatus) oramav1alpha1.EdgeZonePolicyStatus {
	log := logf.FromContext(ctx)
	token, problem := r.token(ctx, policy)
	if problem != "" {
		return failedEdgeStatus(previous, "TokenMissing", problem)
	}
	zone := policy.Spec.ZoneID

	mode, err := r.setting(ctx, token, zone, "ssl")
	if err != nil {
		return failedEdgeStatus(previous, "ZoneSettingsUnreadable", cfErrorMessage("read the zone's settings", err, cfPermissionSettingsRead))
	}
	pulls, err := r.setting(ctx, token, zone, "tls_client_auth")
	if err != nil {
		return failedEdgeStatus(previous, "ZoneSettingsUnreadable", cfErrorMessage("read the zone's settings", err, cfPermissionSettingsRead))
	}
	https, err := r.setting(ctx, token, zone, "always_use_https")
	if err != nil {
		return failedEdgeStatus(previous, "ZoneSettingsUnreadable", cfErrorMessage("read the zone's settings", err, cfPermissionSettingsRead))
	}
	// Automatic SSL/TLS (Cloudflare's default for new zones) chooses the mode itself; it must be
	// "custom" for the mode the operator sets to hold. A zone without the setting answers 404/400.
	automatic, err := r.setting(ctx, token, zone, "ssl_automatic_mode")
	if err != nil && !cfSettingAbsent(err) {
		return failedEdgeStatus(previous, "ZoneSettingsUnreadable", cfErrorMessage("read the zone's settings", err, cfPermissionSettingsRead))
	}
	records, err := r.proxiedRecords(ctx, token, zone)
	if err != nil {
		return failedEdgeStatus(previous, "DNSUnreadable", cfErrorMessage("list the zone's DNS records", err, cfPermissionDNSRead))
	}

	checks := r.probe(ctx, records)
	allTLS, allOrigins, allEdges := true, true, true
	var failing []oramav1alpha1.EdgeZoneHostStatus
	for _, check := range checks {
		allTLS = allTLS && check.handshake
		allOrigins = allOrigins && check.status.Origin == ""
		allEdges = allEdges && check.status.Edge == ""
		if check.status.Origin != "" || check.status.Edge != "" {
			failing = append(failing, check.status)
		}
	}
	sort.Slice(failing, func(i, j int) bool { return failing[i].Name < failing[j].Name })
	if len(failing) > edgeZoneHostLimit {
		failing = failing[:edgeZoneHostLimit]
	}

	// The zone's name, for the Lakes' per-host choice of ACME solver. A refused read does not stop
	// the settings below; the last observed name is kept, and without one the Lakes keep DNS-01 for
	// every name.
	zoneName, zoneNameErr := r.zoneName(ctx, token, zone)
	if zoneNameErr != nil {
		zoneName = previous.Zone
	}

	status := oramav1alpha1.EdgeZonePolicyStatus{
		Zone:         zoneName,
		ProxiedHosts: int32(len(records)),
		Hosts:        failing,
	}
	var changeErrors []string
	conditions := []metav1.Condition{{Type: edgeConditionCredentials, Status: metav1.ConditionTrue, Reason: "Readable", Message: "the zone's settings and DNS records are readable"}}

	// 1 and 2: the origin mode, never weakened.
	target := mode
	switch {
	case allOrigins && mode != edgeOriginStrict:
		target = edgeOriginStrict
	case allTLS && edgeOriginRank(mode) < edgeOriginRank(edgeOriginFull):
		target = edgeOriginFull
	}
	if target != mode {
		if automatic == "auto" {
			if err := r.setSetting(ctx, token, zone, "ssl_automatic_mode", "custom"); err != nil {
				changeErrors = append(changeErrors, cfErrorMessage("switch Automatic SSL/TLS to custom", err, cfPermissionSettingsEdit))
				target = mode
			}
		}
		if target != mode {
			if err := r.setSetting(ctx, token, zone, "ssl", target); err != nil {
				changeErrors = append(changeErrors, cfErrorMessage("set the origin encryption mode to "+target, err, cfPermissionSettingsEdit))
			} else {
				log.Info("zone origin encryption raised", "zone", zone, "from", mode, "to", target)
				mode = target
			}
		}
	}
	status.OriginEncryption = mode
	conditions = append(conditions, edgeCheckCondition(edgeConditionOriginCertificates, allOrigins,
		"every proxied host's origin presents a valid certificate",
		fmt.Sprintf("%d proxied host(s) have no valid origin certificate (status.hosts)", countFailing(checks, true))))
	conditions = append(conditions, edgeCheckCondition(edgeConditionEdgeCertificates, allEdges,
		"every proxied host has a valid edge certificate",
		fmt.Sprintf("%d proxied host(s) have no valid edge certificate (status.hosts)", countFailing(checks, false))))
	conditions = append(conditions, edgeSettingCondition(edgeConditionOriginTLSStrict, mode == edgeOriginStrict,
		"origin encryption is Full (strict)",
		fmt.Sprintf("origin encryption is %q; Full (strict) waits until every proxied host's origin presents a valid certificate", mode)))

	// 3: authenticated origin pulls, once the edge reaches origins over TLS.
	if pulls != "on" && edgeOriginEncrypted(mode) {
		if err := r.setSetting(ctx, token, zone, "tls_client_auth", "on"); err != nil {
			changeErrors = append(changeErrors, cfErrorMessage("turn on authenticated origin pulls", err, cfPermissionSettingsEdit))
		} else {
			log.Info("zone authenticated origin pulls turned on", "zone", zone)
			pulls = "on"
		}
	}
	conditions = append(conditions, edgeSettingCondition(edgeConditionOriginPulls, pulls == "on",
		"the edge presents its client certificate to origins",
		"authenticated origin pulls wait until origin encryption is Full or Full (strict)"))

	// 3b: zone-level authenticated origin pulls with the operator's own certificate (operator#633).
	zoneLevel := r.zoneOriginPulls(ctx, policy, previous.OriginClientCAPEM, token, mode, checks)
	if zoneLevel.changeError != "" {
		changeErrors = append(changeErrors, zoneLevel.changeError)
	}
	status.OriginClientCAPEM = zoneLevel.caBundle
	status.ZoneOriginPulls = zoneLevel.on
	// Origins may require the edge's client certificate only when the bundle verifies whatever the
	// zone presents.
	status.AuthenticatedOriginPulls = (pulls == "on" || zoneLevel.on) && zoneLevel.usable
	conditions = append(conditions, zoneLevel.condition)

	// Edge certificates for hosts Universal SSL does not cover (cloudflare_edge_certificates.go).
	if allEdges {
		conditions = append(conditions, metav1.Condition{Type: edgeConditionEdgeCertificateOrders, Status: metav1.ConditionTrue,
			Reason: "NothingToOrder", Message: "every proxied host has a valid edge certificate"})
	} else {
		conditions = append(conditions, r.orderEdgeCertificates(ctx, token, zone, zoneName, zoneNameErr, checks))
	}

	// 4: always use HTTPS, once every proxied host has an edge certificate.
	if https != "on" && allEdges {
		if err := r.setSetting(ctx, token, zone, "always_use_https", "on"); err != nil {
			changeErrors = append(changeErrors, cfErrorMessage("turn on Always Use HTTPS", err, cfPermissionSettingsEdit))
		} else {
			log.Info("zone always use HTTPS turned on", "zone", zone)
			https = "on"
		}
	}
	status.AlwaysUseHTTPS = https == "on"
	conditions = append(conditions, edgeSettingCondition(edgeConditionAlwaysUseHTTPS, status.AlwaysUseHTTPS,
		"the edge redirects plain HTTP to HTTPS",
		"Always Use HTTPS waits until every proxied host has a valid edge certificate"))

	zoneNameCondition := metav1.Condition{Type: edgeConditionZoneName, Status: metav1.ConditionTrue, Reason: "Read",
		Message: "the zone's name is " + zoneName + "; names outside it are issued through HTTP-01"}
	if zoneNameErr != nil {
		zoneNameCondition.Status, zoneNameCondition.Reason = metav1.ConditionFalse, "Unreadable"
		zoneNameCondition.Message = cfErrorMessage("read the zone's name", zoneNameErr, cfPermissionZoneRead) +
			"; until it is read, every certificate of the zone's Lakes is issued through DNS-01"
		if zoneName != "" {
			zoneNameCondition.Message = cfErrorMessage("read the zone's name", zoneNameErr, cfPermissionZoneRead) +
				"; the last observed name, " + zoneName + ", is kept"
		}
	}

	switch {
	case len(changeErrors) > 0:
		status.Phase = edgePhaseFailed
		status.Message = strings.Join(changeErrors, "; ")
		conditions[0] = metav1.Condition{Type: edgeConditionCredentials, Status: metav1.ConditionFalse, Reason: "ChangeRefused", Message: status.Message}
	case mode == edgeOriginStrict && status.AuthenticatedOriginPulls && status.ZoneOriginPulls && status.AlwaysUseHTTPS:
		status.Phase = edgePhaseReady
		status.Message = "origin encryption Full (strict), zone-level authenticated origin pulls and Always Use HTTPS are on"
	default:
		status.Phase = edgePhaseProgressing
		var waiting []string
		for _, condition := range conditions {
			if condition.Status != metav1.ConditionTrue {
				waiting = append(waiting, condition.Message)
			}
		}
		status.Message = strings.Join(waiting, "; ")
	}
	status.Conditions = append(conditions, zoneNameCondition)
	return status
}

func countFailing(checks []edgeHostCheck, origin bool) int {
	n := 0
	for _, check := range checks {
		if (origin && check.status.Origin != "") || (!origin && check.status.Edge != "") {
			n++
		}
	}
	return n
}

func edgeCheckCondition(kind string, ok bool, yes, no string) metav1.Condition {
	if ok {
		return metav1.Condition{Type: kind, Status: metav1.ConditionTrue, Reason: "Passed", Message: yes}
	}
	return metav1.Condition{Type: kind, Status: metav1.ConditionFalse, Reason: "HostsFailing", Message: no}
}

func edgeSettingCondition(kind string, on bool, yes, no string) metav1.Condition {
	if on {
		return metav1.Condition{Type: kind, Status: metav1.ConditionTrue, Reason: "On", Message: yes}
	}
	return metav1.Condition{Type: kind, Status: metav1.ConditionFalse, Reason: "Waiting", Message: no}
}

// mergeEdgeConditions keeps each condition's last transition time while its status is unchanged,
// so a steady zone writes no status.
func mergeEdgeConditions(previous, next []metav1.Condition, generation int64) []metav1.Condition {
	merged := make([]metav1.Condition, 0, len(next))
	for _, condition := range next {
		condition.ObservedGeneration = generation
		condition.LastTransitionTime = metav1.Now()
		if old := meta.FindStatusCondition(previous, condition.Type); old != nil && old.Status == condition.Status {
			condition.LastTransitionTime = old.LastTransitionTime
		}
		merged = append(merged, condition)
	}
	return merged
}

type edgeHostCheck struct {
	handshake bool
	status    oramav1alpha1.EdgeZoneHostStatus
	// clientCertRequested and acceptableCAs are the origin's CertificateRequest, if it sent one;
	// clientProbeError is set when that could not be learned.
	clientCertRequested bool
	acceptableCAs       [][]byte
	clientProbeError    string
}

// edgeProbeName is the name a record is probed with: itself, or a name under a wildcard.
func edgeProbeName(record string) string {
	if strings.HasPrefix(record, "*.") {
		return edgeProbeLabel + record[1:]
	}
	return record
}

// probe checks every record's origin and edge, eight at a time.
func (r *CloudflareZonePolicyReconciler) probe(ctx context.Context, records []cfDNSRecord) []edgeHostCheck {
	checks := make([]edgeHostCheck, len(records))
	prober := r.prober()
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, record := range records {
		wg.Add(1)
		go func(i int, record cfDNSRecord) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			name := edgeProbeName(record.Name)
			check := edgeHostCheck{status: oramav1alpha1.EdgeZoneHostStatus{Name: record.Name}}
			handshake, err := prober.Origin(ctx, record.Content, name)
			check.handshake = handshake
			if err != nil {
				check.status.Origin = probeMessage(err)
			}
			if err := prober.Edge(ctx, name); err != nil {
				check.status.Edge = probeMessage(err)
			}
			requested, acceptable, err := prober.ClientCAs(ctx, record.Content, name)
			check.clientCertRequested, check.acceptableCAs = requested, acceptable
			if err != nil {
				check.clientProbeError = probeMessage(err)
			}
			checks[i] = check
		}(i, record)
	}
	wg.Wait()
	return checks
}

// probeMessage is a short, stable reason: the same failure must give the same text, or status
// would change on every probe.
func probeMessage(err error) string {
	var hostErr x509.HostnameError
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &hostErr):
		return "certificate is not valid for this name"
	case errors.As(err, &unknown):
		return "certificate is not signed by a trusted authority"
	case errors.As(err, &invalid):
		return "certificate is expired or not yet valid"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "no answer on port 443"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Op == "dial" {
			return "cannot connect on port 443"
		}
		return "TLS handshake failed"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "name does not resolve"
	}
	return "TLS handshake failed"
}

func (r *CloudflareZonePolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&oramav1alpha1.CloudflareZonePolicy{}).Complete(r)
}

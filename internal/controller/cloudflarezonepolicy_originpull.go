/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Zone-level authenticated origin pulls with an operator-owned certificate (operator#633).
//
// Global authenticated origin pulls (the zone setting tls_client_auth) make Cloudflare present a
// certificate it shares with every Cloudflare account: it proves "from Cloudflare", not "from our
// zone". Zone-level authenticated origin pulls present a client certificate the zone uploaded, and
// take precedence over the global one. Each zone has one CA and client leaf, made by the managing
// cluster's operator and copied by the LakeSet to every cluster serving the zone
// (managedset_origin_pull.go); the policy uploads the leaf, turns zone-level pulls on, and publishes
// the CA for the origins to verify against. A policy with no distributed Secret (a Lake no set
// manages) uses the operator's own Secret, which it creates and renews.
//
// The danger is the switch itself: once the zone presents our leaf, an origin that requires a
// client certificate and does not trust our CA refuses Cloudflare. A zone may have origins the
// operator does not run, or a cluster not yet given the zone's CA. So the leaf is uploaded
// and zone-level pulls are turned on only when a probe of every proxied origin shows that each one
// that asks for a client certificate lists our CA among the CAs it accepts (the TLS
// CertificateRequest's certificate_authorities). Until then the origins trust Cloudflare's global
// CA and ours together, so the switch is safe from either side.
const (
	edgeConditionZoneOriginPulls = "ZoneOriginPulls"

	originPullCALifetime   = 10 * 365 * 24 * time.Hour
	originPullLeafLifetime = 365 * 24 * time.Hour
	// originPullRenewBefore is when a leaf is replaced; Cloudflare's own expiry alerts start 30
	// days out.
	originPullRenewBefore = 45 * 24 * time.Hour
	// originPullBlockedLimit bounds how many hosts a waiting condition names.
	originPullBlockedLimit = 5
)

var originPullZoneIDPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,200}[a-z0-9])?$`)

// originPullSecretName is the Secret holding one zone's CA and leaf: on the managing cluster in the
// operator's namespace, and as a distributed copy in each set member's namespace. Every policy
// naming the zone shares it, so they upload the same leaf.
func originPullSecretName(zoneID string) string {
	id := strings.ToLower(zoneID)
	if !originPullZoneIDPattern.MatchString(id) {
		sum := sha256.Sum256([]byte(zoneID))
		id = hex.EncodeToString(sum[:])[:32]
	}
	return "orama-edge-origin-pull-" + id
}

// originPullCredentials are a zone's CA and current client leaf.
type originPullCredentials struct {
	caPEM   string
	ca      *x509.Certificate
	leafPEM string
	keyPEM  string
	leaf    *x509.Certificate
}

func originPullSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func pemBlock(kind string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
}

func newOriginPullCA(zoneID string, now time.Time) (certPEM, keyPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	serial, err := originPullSerial()
	if err != nil {
		return "", "", err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		// The subject is unique to this CA (its serial number is in it): an origin's
		// CertificateRequest names the CAs it accepts by subject alone, so two CAs of one zone with
		// the same subject would let the probe take one for the other.
		Subject:               pkix.Name{Organization: []string{"Orama"}, CommonName: "orama-operator origin-pull CA " + zoneID, SerialNumber: serial.Text(16)},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(originPullCALifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	return pemBlock("CERTIFICATE", der), pemBlock("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key)), nil
}

func newOriginPullLeaf(zoneID string, ca *x509.Certificate, caKey *rsa.PrivateKey, now time.Time) (certPEM, keyPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	serial, err := originPullSerial()
	if err != nil {
		return "", "", err
	}
	notAfter := now.Add(originPullLeafLifetime)
	if notAfter.After(ca.NotAfter) {
		notAfter = ca.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"Orama"}, CommonName: "orama-operator origin-pull " + zoneID},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		return "", "", err
	}
	return pemBlock("CERTIFICATE", der), pemBlock("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key)), nil
}

func parsePEMCertificate(data string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(data))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parsePEMRSAKey(data string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(data))
	if block == nil {
		return nil, errors.New("no PEM key")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// originPullDistributedLabel marks a copy of a zone's origin-pull Secret that a set distributed
// to a member cluster. A copy carries the CA certificate and the client certificate, never the CA
// key: a member cluster cannot sign a client certificate of its own.
const originPullDistributedLabel = "orama.cloud/edge-origin-pull"

// ensureOriginPullSecret returns the zone's origin-pull Secret in namespace and its CA and a leaf
// valid for at least originPullRenewBefore, creating the Secret (with labels), or a new leaf in it,
// when needed. The CA is never replaced: a Secret whose CA cannot be read is an error, not a reason
// to make another (a new CA would break every origin that trusts the old one). The zone policy
// calls it for the cluster's own Secret, and a LakeSet on the managing cluster for the Secret it
// distributes; on the managing cluster both are the same Secret.
func ensureOriginPullSecret(ctx context.Context, c client.Client, namespace, zoneID string, labels map[string]string, now time.Time) (*corev1.Secret, *originPullCredentials, error) {
	name := originPullSecretName(zoneID)
	var secret corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &secret)
	if apierrors.IsNotFound(err) {
		caPEM, caKeyPEM, genErr := newOriginPullCA(zoneID, now)
		if genErr != nil {
			return nil, nil, genErr
		}
		all := map[string]string{
			"app.kubernetes.io/managed-by": "orama-operator",
			"orama.cloud/edge-zone":        strings.ToLower(zoneID),
		}
		for k, v := range labels {
			all[k] = v
		}
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: all},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"ca.crt": []byte(caPEM), "ca.key": []byte(caKeyPEM)},
		}
		if err := addOriginPullLeaf(&secret, zoneID, now); err != nil {
			return nil, nil, err
		}
		if err := c.Create(ctx, &secret); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return nil, nil, err
			}
			if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &secret); err != nil {
				return nil, nil, err
			}
		} else {
			logf.FromContext(ctx).Info("zone origin-pull CA created", "zone", zoneID, "secret", name)
		}
	} else if err != nil {
		return nil, nil, err
	}
	creds, problem := readOriginPullCredentials(&secret)
	if creds == nil {
		return nil, nil, fmt.Errorf("secret %s/%s: %s", namespace, name, problem)
	}
	if problem != "" || now.Add(originPullRenewBefore).After(creds.leaf.NotAfter) {
		if err := addOriginPullLeaf(&secret, zoneID, now); err != nil {
			return nil, nil, err
		}
		if err := c.Update(ctx, &secret); err != nil {
			return nil, nil, err
		}
		logf.FromContext(ctx).Info("zone origin-pull client certificate renewed", "zone", zoneID, "secret", name)
		if creds, problem = readOriginPullCredentials(&secret); creds == nil || problem != "" {
			return nil, nil, fmt.Errorf("secret %s/%s: %s", namespace, name, problem)
		}
	}
	return &secret, creds, nil
}

// distributedOriginPullCopy is what a set copies to a member: the CA certificate and the current
// client certificate, without the CA key.
func distributedOriginPullCopy(source *corev1.Secret) *corev1.Secret {
	copied := &corev1.Secret{Type: corev1.SecretTypeOpaque, Data: map[string][]byte{
		"ca.crt": source.Data["ca.crt"], "tls.crt": source.Data["tls.crt"], "tls.key": source.Data["tls.key"],
	}}
	copied.Name = source.Name
	copied.Labels = map[string]string{
		"app.kubernetes.io/managed-by": "orama-operator",
		"orama.cloud/edge-zone":        source.Labels["orama.cloud/edge-zone"],
		originPullDistributedLabel:     "distributed",
	}
	return copied
}

// readDistributedOriginPull reads a zone's distributed origin-pull Secret. It returns wait when the
// Secret is not there yet or holds no usable client certificate (the managing cluster writes it;
// this cluster never does), and an error when the CA cannot be read.
func readDistributedOriginPull(ctx context.Context, c client.Client, namespace, name string, now time.Time) (creds *originPullCredentials, wait string, err error) {
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Sprintf("waiting for the zone's origin-pull Secret %s/%s, which the managing cluster's set copies here; this cluster never creates a CA for the zone", namespace, name), nil
		}
		return nil, "", err
	}
	caPEM := string(secret.Data["ca.crt"])
	ca, err := parsePEMCertificate(caPEM)
	if err != nil || !ca.IsCA {
		return nil, "", fmt.Errorf("secret %s/%s: ca.crt is not a CA certificate", namespace, name)
	}
	creds = &originPullCredentials{caPEM: caPEM, ca: ca, leafPEM: string(secret.Data["tls.crt"]), keyPEM: string(secret.Data["tls.key"])}
	leaf, err := parsePEMCertificate(creds.leafPEM)
	switch {
	case err != nil:
		return nil, fmt.Sprintf("the zone's origin-pull Secret %s/%s has no client certificate yet", namespace, name), nil
	case leaf.CheckSignatureFrom(ca) != nil:
		return nil, fmt.Sprintf("the client certificate in %s/%s is not signed by its CA; waiting for the managing cluster to copy a new one", namespace, name), nil
	case !now.Before(leaf.NotAfter):
		return nil, fmt.Sprintf("the client certificate in %s/%s expired; waiting for the managing cluster to copy a renewed one", namespace, name), nil
	}
	if _, err := parsePEMRSAKey(creds.keyPEM); err != nil {
		return nil, fmt.Sprintf("the zone's origin-pull Secret %s/%s has no client key yet", namespace, name), nil
	}
	creds.leaf = leaf
	return creds, "", nil
}

// addOriginPullLeaf writes a new leaf signed by the Secret's CA.
func addOriginPullLeaf(secret *corev1.Secret, zoneID string, now time.Time) error {
	ca, err := parsePEMCertificate(string(secret.Data["ca.crt"]))
	if err != nil {
		return fmt.Errorf("read the CA: %w", err)
	}
	caKey, err := parsePEMRSAKey(string(secret.Data["ca.key"]))
	if err != nil {
		return fmt.Errorf("read the CA key: %w", err)
	}
	certPEM, keyPEM, err := newOriginPullLeaf(zoneID, ca, caKey, now)
	if err != nil {
		return err
	}
	secret.Data["tls.crt"], secret.Data["tls.key"] = []byte(certPEM), []byte(keyPEM)
	return nil
}

// readOriginPullCredentials returns nil and a reason when the CA cannot be used, and credentials
// with a reason when only the leaf must be replaced.
func readOriginPullCredentials(secret *corev1.Secret) (*originPullCredentials, string) {
	caPEM := string(secret.Data["ca.crt"])
	ca, err := parsePEMCertificate(caPEM)
	if err != nil || !ca.IsCA {
		return nil, "ca.crt is not a CA certificate"
	}
	if _, err := parsePEMRSAKey(string(secret.Data["ca.key"])); err != nil {
		return nil, "ca.key is not an RSA key"
	}
	creds := &originPullCredentials{caPEM: caPEM, ca: ca, leafPEM: string(secret.Data["tls.crt"]), keyPEM: string(secret.Data["tls.key"])}
	leaf, err := parsePEMCertificate(creds.leafPEM)
	if err != nil {
		return creds, "tls.crt is missing"
	}
	if leaf.CheckSignatureFrom(ca) != nil {
		return creds, "tls.crt is not signed by ca.crt"
	}
	if _, err := parsePEMRSAKey(creds.keyPEM); err != nil {
		return creds, "tls.key is missing"
	}
	creds.leaf = leaf
	return creds, ""
}

// cfZoneClientCert is a zone-level authenticated origin pull certificate.
type cfZoneClientCert struct {
	ID          string `json:"id"`
	Certificate string `json:"certificate"`
	Status      string `json:"status"`
	ExpiresOn   string `json:"expires_on,omitempty"`
}

type cfZoneClientCertList struct {
	Result []cfZoneClientCert `json:"result"`
}

type cfZoneClientCertResponse struct {
	Result cfZoneClientCert `json:"result"`
}

type cfZoneClientAuthSettings struct {
	Result struct {
		Enabled bool `json:"enabled"`
	} `json:"result"`
}

// cfClientCertLive is whether Cloudflare may present the certificate, now or once deployed.
func cfClientCertLive(status string) bool {
	switch status {
	case "active", "pending_deployment", "initializing":
		return true
	}
	return false
}

// zoneOriginPullResult is what the zone-level step observed and did.
type zoneOriginPullResult struct {
	// caBundle is what the origins verify the edge's client certificate against.
	caBundle string
	// usable is false when the zone may present a client certificate the bundle cannot verify, so
	// the origins must not require one.
	usable bool
	// on is true when the zone presents our leaf.
	on        bool
	condition metav1.Condition
	// changeError is a refused change, for the policy's phase.
	changeError string
}

// clientCAAccepted is whether an origin accepts ca as the issuer of client certificates.
func clientCAAccepted(acceptable [][]byte, ca *x509.Certificate) bool {
	for _, name := range acceptable {
		if bytes.Equal(name, ca.RawSubject) {
			return true
		}
	}
	return false
}

// originsNotTrusting lists the proxied hosts whose origin asks for a client certificate without
// accepting ca, or could not be probed.
func originsNotTrusting(checks []edgeHostCheck, ca *x509.Certificate) []string {
	var hosts []string
	for _, check := range checks {
		switch {
		case check.clientProbeError != "":
			hosts = append(hosts, check.status.Name+" (no answer)")
		case check.clientCertRequested && !clientCAAccepted(check.acceptableCAs, ca):
			hosts = append(hosts, check.status.Name)
		}
	}
	sort.Strings(hosts)
	return hosts
}

// zoneCredentials returns the zone's CA and client certificate, and where they are, following the
// precedence: a distributed Secret named by the policy is the only source when set (this cluster
// never mints a CA or a leaf for the zone, and waits while it is missing); otherwise the operator's
// own Secret in its namespace, created once.
func (r *CloudflareZonePolicyReconciler) zoneCredentials(ctx context.Context, policy *oramav1alpha1.CloudflareZonePolicy) (creds *originPullCredentials, where, wait string, err error) {
	if ref := policy.Spec.OriginPullSecretRef; ref != nil && ref.Name != "" {
		where = policy.Namespace + "/" + ref.Name
		creds, wait, err = readDistributedOriginPull(ctx, r.Client, policy.Namespace, ref.Name, r.now())
		return creds, where, wait, err
	}
	namespace := r.OperatorNamespace
	if namespace == "" {
		namespace = policy.Namespace
	}
	where = namespace + "/" + originPullSecretName(policy.Spec.ZoneID)
	_, creds, err = ensureOriginPullSecret(ctx, r.Client, namespace, policy.Spec.ZoneID, nil, r.now())
	return creds, where, "", err
}

// joinPEM joins PEM certificates, each on its own lines. Cloudflare's published CA file (embedded
// as is) has no final newline, and a bundle whose END line runs into the next BEGIN line is not
// PEM: nginx and Go's decoder read no certificate from it at all.
func joinPEM(parts ...string) string {
	var b strings.Builder
	for _, part := range parts {
		b.WriteString(part)
		if part != "" && !strings.HasSuffix(part, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ourZoneCert is a zone-level certificate signed by the zone's CA.
type ourZoneCert struct {
	cert cfZoneClientCert
	leaf *x509.Certificate
}

func (r *CloudflareZonePolicyReconciler) zoneOriginPulls(ctx context.Context, policy *oramav1alpha1.CloudflareZonePolicy, previousBundle, token, mode string, checks []edgeHostCheck) zoneOriginPullResult {
	log := logf.FromContext(ctx)
	zone := policy.Spec.ZoneID
	waiting := func(reason, message string) metav1.Condition {
		return metav1.Condition{Type: edgeConditionZoneOriginPulls, Status: metav1.ConditionFalse, Reason: reason, Message: message}
	}
	// Without the zone's credentials the bundle stays what the origins were given last: dropping the
	// zone's CA from it would make them refuse a zone that already presents its certificate.
	kept := previousBundle
	if kept == "" {
		kept = cloudflareOriginPullCA
	}
	creds, where, wait, err := r.zoneCredentials(ctx, policy)
	if err != nil {
		message := "cannot prepare the zone's origin-pull certificate: " + err.Error()
		return zoneOriginPullResult{caBundle: kept, usable: true, condition: waiting("CertificateUnavailable", message), changeError: message}
	}
	if wait != "" {
		return zoneOriginPullResult{caBundle: kept, usable: true, condition: waiting("WaitingForDistributedCA", wait)}
	}
	// Until the zone presents a leaf of the zone's CA, origins accept Cloudflare's global
	// certificate and the zone's CA.
	both := zoneOriginPullResult{caBundle: joinPEM(cloudflareOriginPullCA, creds.caPEM), usable: true}
	if !edgeOriginEncrypted(mode) {
		both.condition = waiting("Waiting", "zone-level authenticated origin pulls wait until origin encryption is Full or Full (strict)")
		return both
	}
	base := fmt.Sprintf("%s/zones/%s/origin_tls_client_auth", r.api(), zone)
	var list cfZoneClientCertList
	if err := cfDo(ctx, token, http.MethodGet, base, nil, &list); err != nil {
		both.condition = waiting("ZoneCertificatesUnreadable", cfErrorMessage("read the zone's origin-pull certificates", err, cfPermissionCertificatesRead))
		return both
	}
	var settings cfZoneClientAuthSettings
	if err := cfDo(ctx, token, http.MethodGet, base+"/settings", nil, &settings); err != nil {
		both.condition = waiting("ZoneCertificatesUnreadable", cfErrorMessage("read the zone's origin-pull setting", err, cfPermissionCertificatesRead))
		return both
	}
	enabled := settings.Result.Enabled

	// Every cluster serving the zone holds the same CA, and may hold a different leaf of it for a
	// while (the managing cluster renewed it and the copy has not reached this cluster yet). The
	// newest live leaf of the CA is the zone's current one, whoever uploaded it: a cluster uploads its
	// own leaf only when it is newer, and deletes only older leaves, so clusters never undo each
	// other.
	oursActive, foreignLive := false, false
	var ours []ourZoneCert
	newest := -1
	for _, cert := range list.Result {
		parsed, err := parsePEMCertificate(cert.Certificate)
		if err != nil || parsed.CheckSignatureFrom(creds.ca) != nil {
			if cfClientCertLive(cert.Status) {
				foreignLive = true
			}
			continue
		}
		oursActive = oursActive || cert.Status == "active"
		ours = append(ours, ourZoneCert{cert: cert, leaf: parsed})
		if !cfClientCertLive(cert.Status) {
			continue
		}
		if newest < 0 || parsed.NotAfter.After(ours[newest].leaf.NotAfter) ||
			(parsed.NotAfter.Equal(ours[newest].leaf.NotAfter) && parsed.Equal(creds.leaf) && !ours[newest].leaf.Equal(creds.leaf)) {
			newest = len(ours) - 1
		}
	}
	var current *ourZoneCert
	if newest >= 0 {
		current = &ours[newest]
	}

	// A change that makes the zone present a leaf of the zone's CA where it did not before needs
	// every origin that asks for a client certificate to accept the CA. Renewing a leaf does not.
	if !(enabled && oursActive && !foreignLive) {
		if blocked := originsNotTrusting(checks, creds.ca); len(blocked) > 0 {
			more := ""
			if len(blocked) > originPullBlockedLimit {
				more = fmt.Sprintf(" and %d more", len(blocked)-originPullBlockedLimit)
				blocked = blocked[:originPullBlockedLimit]
			}
			both.condition = waiting("OriginsDoNotTrustCA", fmt.Sprintf(
				"zone-level authenticated origin pulls wait until every proxied origin that asks for a client certificate accepts the zone's origin-pull CA (Secret %s); not yet: %s%s",
				where, strings.Join(blocked, ", "), more))
			both.usable = !(enabled && foreignLive)
			return both
		}
	}

	if current == nil || creds.leaf.NotAfter.After(current.leaf.NotAfter) {
		var out cfZoneClientCertResponse
		if err := cfDo(ctx, token, http.MethodPost, base, map[string]string{"certificate": creds.leafPEM, "private_key": creds.keyPEM}, &out); err != nil {
			message := cfErrorMessage("upload the zone's origin-pull certificate", err, cfPermissionCertificatesEdit)
			both.condition = waiting("ChangeRefused", message)
			both.changeError = message
			both.usable = !(enabled && foreignLive)
			return both
		}
		log.Info("zone origin-pull certificate uploaded", "zone", zone, "id", out.Result.ID)
		current = &ourZoneCert{cert: out.Result, leaf: creds.leaf}
	}
	if !enabled {
		if err := cfDo(ctx, token, http.MethodPut, base+"/settings", map[string]bool{"enabled": true}, nil); err != nil {
			message := cfErrorMessage("turn on zone-level authenticated origin pulls", err, cfPermissionCertificatesEdit)
			both.condition = waiting("ChangeRefused", message)
			both.changeError = message
			return both
		}
		log.Info("zone-level authenticated origin pulls turned on", "zone", zone)
	}
	if current.cert.Status != "active" {
		if oursActive && !foreignLive {
			// Renewing: the zone presents the previous leaf of the CA until the new one is active.
			return zoneOriginPullResult{
				caBundle: creds.caPEM, usable: true, on: true,
				condition: metav1.Condition{Type: edgeConditionZoneOriginPulls, Status: metav1.ConditionTrue, Reason: "Renewing",
					Message: fmt.Sprintf("the edge presents the zone's own client certificate; its replacement %s is %s", current.cert.ID, current.cert.Status)},
			}
		}
		// Deploying: the zone presents the previous certificate (Cloudflare's global one) until
		// this one is active, and the bundle accepts both.
		both.condition = waiting("Deploying", fmt.Sprintf("the zone's origin-pull certificate %s is %s", current.cert.ID, current.cert.Status))
		both.usable = !foreignLive
		return both
	}
	// The current leaf is active: older leaves of the CA, and second copies of the current one, are
	// removed. A certificate of another CA is never touched.
	for _, old := range ours {
		if old.cert.ID == current.cert.ID || old.cert.Status == "pending_deletion" || old.cert.Status == "deleted" {
			continue
		}
		if !old.leaf.NotAfter.Before(current.leaf.NotAfter) && !old.leaf.Equal(current.leaf) {
			continue
		}
		err := cfDo(ctx, token, http.MethodDelete, base+"/"+old.cert.ID, nil, nil)
		var apiErr *cfAPIError
		if err != nil && !(errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound) {
			both.condition = waiting("ChangeRefused", cfErrorMessage("delete a replaced origin-pull certificate", err, cfPermissionCertificatesEdit))
			both.changeError = both.condition.Message
			return both
		}
		log.Info("replaced zone origin-pull certificate deleted", "zone", zone, "id", old.cert.ID)
	}
	if foreignLive {
		both.condition = waiting("ForeignCertificate", "the zone also holds an origin-pull certificate of another CA, which Cloudflare may present; origins do not require a client certificate until it is gone")
		both.usable = false
		return both
	}
	return zoneOriginPullResult{
		caBundle: creds.caPEM,
		usable:   true,
		on:       true,
		condition: metav1.Condition{Type: edgeConditionZoneOriginPulls, Status: metav1.ConditionTrue, Reason: "On",
			Message: "the edge presents the zone's own client certificate, signed by the zone's origin-pull CA (Secret " + where + ")"},
	}
}

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
)

// Zone-level authenticated origin pulls with an operator-owned certificate (operator#633).
//
// Global authenticated origin pulls (the zone setting tls_client_auth) make Cloudflare present a
// certificate it shares with every Cloudflare account: it proves "from Cloudflare", not "from our
// zone". Zone-level authenticated origin pulls present a client certificate the zone uploaded, and
// take precedence over the global one. The operator generates a CA and a client leaf per zone,
// uploads the leaf, turns zone-level pulls on, rotates the leaf before it expires, and publishes the
// CA for the origins to verify against.
//
// The danger is the switch itself: once the zone presents our leaf, an origin that requires a
// client certificate and does not trust our CA refuses Cloudflare. A zone may be served by several
// clusters, each with its own CA, and by origins the operator does not run. So the leaf is uploaded
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

// originPullSecretName is the Secret holding one zone's CA and leaf: one per zone per cluster,
// shared by every policy naming the zone, so they upload the same leaf.
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
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"Orama"}, CommonName: "orama-operator origin-pull CA " + zoneID},
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

// originPullCredentials returns the zone's CA and a leaf valid for at least originPullRenewBefore,
// creating the Secret, or a new leaf in it, when needed. The CA is never replaced: a Secret whose
// CA cannot be read is an error, not a reason to make another (a new CA would break every origin
// that trusts the old one).
func (r *CloudflareZonePolicyReconciler) originPullCredentials(ctx context.Context, namespace, zoneID string, now time.Time) (*originPullCredentials, error) {
	name := originPullSecretName(zoneID)
	var secret corev1.Secret
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &secret)
	if apierrors.IsNotFound(err) {
		caPEM, caKeyPEM, genErr := newOriginPullCA(zoneID, now)
		if genErr != nil {
			return nil, genErr
		}
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{
				"app.kubernetes.io/managed-by": "orama-operator",
				"orama.cloud/edge-zone":        strings.ToLower(zoneID),
			}},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{"ca.crt": []byte(caPEM), "ca.key": []byte(caKeyPEM)},
		}
		if err := r.addOriginPullLeaf(&secret, zoneID, now); err != nil {
			return nil, err
		}
		if err := r.Create(ctx, &secret); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return nil, err
			}
			if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &secret); err != nil {
				return nil, err
			}
		}
		logf.FromContext(ctx).Info("zone origin-pull CA created", "zone", zoneID, "secret", name)
	} else if err != nil {
		return nil, err
	}
	creds, problem := readOriginPullCredentials(&secret)
	if creds == nil {
		return nil, fmt.Errorf("secret %s/%s: %s", namespace, name, problem)
	}
	if problem != "" || now.Add(originPullRenewBefore).After(creds.leaf.NotAfter) {
		if err := r.addOriginPullLeaf(&secret, zoneID, now); err != nil {
			return nil, err
		}
		if err := r.Update(ctx, &secret); err != nil {
			return nil, err
		}
		logf.FromContext(ctx).Info("zone origin-pull client certificate renewed", "zone", zoneID, "secret", name)
		if creds, problem = readOriginPullCredentials(&secret); creds == nil || problem != "" {
			return nil, fmt.Errorf("secret %s/%s: %s", namespace, name, problem)
		}
	}
	return creds, nil
}

// addOriginPullLeaf writes a new leaf signed by the Secret's CA.
func (r *CloudflareZonePolicyReconciler) addOriginPullLeaf(secret *corev1.Secret, zoneID string, now time.Time) error {
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

func (r *CloudflareZonePolicyReconciler) zoneOriginPulls(ctx context.Context, policyNamespace, token, zone, mode string, checks []edgeHostCheck) zoneOriginPullResult {
	log := logf.FromContext(ctx)
	waiting := func(reason, message string) metav1.Condition {
		return metav1.Condition{Type: edgeConditionZoneOriginPulls, Status: metav1.ConditionFalse, Reason: reason, Message: message}
	}
	namespace := r.OperatorNamespace
	if namespace == "" {
		namespace = policyNamespace
	}
	creds, err := r.originPullCredentials(ctx, namespace, zone, r.now())
	if err != nil {
		message := "cannot prepare the zone's origin-pull certificate: " + err.Error()
		return zoneOriginPullResult{caBundle: cloudflareOriginPullCA, usable: true, condition: waiting("CertificateUnavailable", message), changeError: message}
	}
	// Until the zone presents our leaf, origins accept Cloudflare's global certificate and ours.
	both := zoneOriginPullResult{caBundle: cloudflareOriginPullCA + creds.caPEM, usable: true}
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

	var current *cfZoneClientCert
	oursActive, foreignLive := false, false
	var oldOurs []cfZoneClientCert
	for i := range list.Result {
		cert := list.Result[i]
		parsed, err := parsePEMCertificate(cert.Certificate)
		ours := err == nil && parsed.CheckSignatureFrom(creds.ca) == nil
		switch {
		case ours && parsed.Equal(creds.leaf) && cfClientCertLive(cert.Status):
			current = &list.Result[i]
			oursActive = oursActive || cert.Status == "active"
		case ours:
			oursActive = oursActive || cert.Status == "active"
			oldOurs = append(oldOurs, cert)
		case cfClientCertLive(cert.Status):
			foreignLive = true
		}
	}

	// A change that makes the zone present our leaf where it did not before needs every origin
	// that asks for a client certificate to accept our CA. Renewing a leaf of our own CA does not.
	if !(enabled && oursActive && !foreignLive) {
		if blocked := originsNotTrusting(checks, creds.ca); len(blocked) > 0 {
			more := ""
			if len(blocked) > originPullBlockedLimit {
				more = fmt.Sprintf(" and %d more", len(blocked)-originPullBlockedLimit)
				blocked = blocked[:originPullBlockedLimit]
			}
			both.condition = waiting("OriginsDoNotTrustCA", fmt.Sprintf(
				"zone-level authenticated origin pulls wait until every proxied origin that asks for a client certificate accepts this cluster's origin-pull CA (Secret %s/%s); not yet: %s%s",
				namespace, originPullSecretName(zone), strings.Join(blocked, ", "), more))
			both.usable = !(enabled && foreignLive)
			return both
		}
	}

	if current == nil {
		var out cfZoneClientCertResponse
		if err := cfDo(ctx, token, http.MethodPost, base, map[string]string{"certificate": creds.leafPEM, "private_key": creds.keyPEM}, &out); err != nil {
			message := cfErrorMessage("upload the zone's origin-pull certificate", err, cfPermissionCertificatesEdit)
			both.condition = waiting("ChangeRefused", message)
			both.changeError = message
			both.usable = !(enabled && foreignLive)
			return both
		}
		log.Info("zone origin-pull certificate uploaded", "zone", zone, "id", out.Result.ID)
		current = &out.Result
	}
	if !enabled {
		if err := cfDo(ctx, token, http.MethodPut, base+"/settings", map[string]bool{"enabled": true}, nil); err != nil {
			message := cfErrorMessage("turn on zone-level authenticated origin pulls", err, cfPermissionCertificatesEdit)
			both.condition = waiting("ChangeRefused", message)
			both.changeError = message
			return both
		}
		log.Info("zone-level authenticated origin pulls turned on", "zone", zone)
		enabled = true
	}
	if current.Status != "active" {
		if oursActive && !foreignLive {
			// Renewing: the zone presents the previous leaf of our CA until the new one is active.
			return zoneOriginPullResult{
				caBundle: creds.caPEM, usable: true, on: true,
				condition: metav1.Condition{Type: edgeConditionZoneOriginPulls, Status: metav1.ConditionTrue, Reason: "Renewing",
					Message: fmt.Sprintf("the edge presents the zone's own client certificate; its replacement %s is %s", current.ID, current.Status)},
			}
		}
		// Deploying: the zone presents the previous certificate (Cloudflare's global one) until
		// this one is active, and the bundle accepts both.
		both.condition = waiting("Deploying", fmt.Sprintf("the zone's origin-pull certificate %s is %s", current.ID, current.Status))
		both.usable = !foreignLive
		return both
	}
	// Ours is active: older leaves of our CA are removed. A certificate of another CA is never
	// touched.
	for _, old := range oldOurs {
		if old.Status == "pending_deletion" || old.Status == "deleted" {
			continue
		}
		err := cfDo(ctx, token, http.MethodDelete, base+"/"+old.ID, nil, nil)
		var apiErr *cfAPIError
		if err != nil && !(errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound) {
			both.condition = waiting("ChangeRefused", cfErrorMessage("delete a replaced origin-pull certificate", err, cfPermissionCertificatesEdit))
			both.changeError = both.condition.Message
			return both
		}
		log.Info("replaced zone origin-pull certificate deleted", "zone", zone, "id", old.ID)
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
			Message: "the edge presents the zone's own client certificate, signed by the operator's CA"},
	}
}

/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"slices"
	"sort"
	"time"
)

// The operator's own certificate authorities for zetta-tunnel (relay, endpoints, peer gateways and
// the mesh). Keys are ECDSA P-256; TLS 1.3 is all zetta-tunnel speaks.
const (
	tunnelCAValidity   = 10 * 365 * 24 * time.Hour
	tunnelLeafValidity = 90 * 24 * time.Hour
	// A leaf is reissued once less than this remains, or when its names or issuer change.
	tunnelLeafRenewBefore = 30 * 24 * time.Hour
)

type tunnelCA struct {
	CertPEM []byte
	KeyPEM  []byte
}

// tunnelLeafRequest is what a leaf certificate must say.
type tunnelLeafRequest struct {
	CommonName string
	DNSNames   []string
	IPs        []net.IP
	URIs       []string
	Server     bool
	Client     bool
}

func newTunnelCA(commonName string, now time.Time) (tunnelCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tunnelCA{}, err
	}
	serial, err := tunnelSerial()
	if err != nil {
		return tunnelCA{}, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"Orama"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(tunnelCAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tunnelCA{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tunnelCA{}, err
	}
	return tunnelCA{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// newUnusableTunnelCA is a CA certificate whose key is discarded at once. It keeps a trust bundle
// valid and non-empty when no real CA is in it, while trusting nothing.
func newUnusableTunnelCA(commonName string, now time.Time) ([]byte, error) {
	ca, err := newTunnelCA(commonName, now)
	if err != nil {
		return nil, err
	}
	return ca.CertPEM, nil
}

func tunnelSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
}

func parseTunnelCA(ca tunnelCA) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(ca.CertPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, nil, errors.New("CA certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	keyBlock, _ := pem.Decode(ca.KeyPEM)
	if keyBlock == nil {
		return nil, nil, errors.New("CA key is not PEM")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, nil, errors.New("CA key does not match its certificate")
	}
	return cert, key, nil
}

// issueTunnelLeaf signs a leaf with ca. It returns the certificate and its new key, both PEM.
func issueTunnelLeaf(ca tunnelCA, request tunnelLeafRequest, now time.Time) ([]byte, []byte, error) {
	caCert, caKey, err := parseTunnelCA(ca)
	if err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := tunnelSerial()
	if err != nil {
		return nil, nil, err
	}
	var uris []*url.URL
	for _, raw := range request.URIs {
		parsed, err := url.Parse(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("URI SAN %q: %w", raw, err)
		}
		uris = append(uris, parsed)
	}
	var usages []x509.ExtKeyUsage
	if request.Server {
		usages = append(usages, x509.ExtKeyUsageServerAuth)
	}
	if request.Client {
		usages = append(usages, x509.ExtKeyUsageClientAuth)
	}
	notAfter := now.Add(tunnelLeafValidity)
	if notAfter.After(caCert.NotAfter) {
		notAfter = caCert.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: request.CommonName, Organization: []string{"Orama"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           usages,
		BasicConstraintsValid: true,
		DNSNames:              append([]string(nil), request.DNSNames...),
		IPAddresses:           append([]net.IP(nil), request.IPs...),
		URIs:                  uris,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		nil
}

// tunnelLeafCurrent reports whether certPEM/keyPEM are a matching pair, signed by caPEM, say
// exactly what request asks for, and have more than tunnelLeafRenewBefore left.
func tunnelLeafCurrent(certPEM, keyPEM, caPEM []byte, request tunnelLeafRequest, now time.Time) bool {
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	caBlock, _ := pem.Decode(caPEM)
	if certBlock == nil || keyBlock == nil || caBlock == nil {
		return false
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return false
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil || !key.PublicKey.Equal(cert.PublicKey) {
		return false
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil || cert.CheckSignatureFrom(caCert) != nil {
		return false
	}
	if now.Before(cert.NotBefore) || cert.NotAfter.Sub(now) < tunnelLeafRenewBefore &&
		cert.NotAfter.Before(caCert.NotAfter) {
		return false
	}
	if !sameStrings(cert.DNSNames, request.DNSNames) {
		return false
	}
	var ips, wantIPs []string
	for _, ip := range cert.IPAddresses {
		ips = append(ips, ip.String())
	}
	for _, ip := range request.IPs {
		wantIPs = append(wantIPs, ip.String())
	}
	if !sameStrings(ips, wantIPs) {
		return false
	}
	var uris []string
	for _, uri := range cert.URIs {
		uris = append(uris, uri.String())
	}
	if !sameStrings(uris, request.URIs) {
		return false
	}
	wantUsages := []x509.ExtKeyUsage{}
	if request.Server {
		wantUsages = append(wantUsages, x509.ExtKeyUsageServerAuth)
	}
	if request.Client {
		wantUsages = append(wantUsages, x509.ExtKeyUsageClientAuth)
	}
	return slices.Equal(cert.ExtKeyUsage, wantUsages)
}

func sameStrings(a, b []string) bool {
	a = append([]string(nil), a...)
	b = append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	return slices.Equal(a, b)
}

// pemCertificates returns each CERTIFICATE block in data, in order, and an error if data holds
// anything else or none.
func pemCertificates(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := bytes.TrimSpace(data)
	for len(rest) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("not PEM")
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("unexpected PEM block %q", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
		rest = bytes.TrimSpace(rest)
	}
	if len(certs) == 0 {
		return nil, errors.New("no certificate")
	}
	return certs, nil
}

// pemBundle concatenates distinct certificates in a stable order.
func pemBundle(certs ...[]byte) []byte {
	seen := map[string]bool{}
	var keys []string
	byKey := map[string][]byte{}
	for _, data := range certs {
		parsed, err := pemCertificates(data)
		if err != nil {
			continue
		}
		for _, cert := range parsed {
			sum := sha256.Sum256(cert.Raw)
			key := hex.EncodeToString(sum[:])
			if seen[key] {
				continue
			}
			seen[key] = true
			keys = append(keys, key)
			byKey[key] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
		}
	}
	sort.Strings(keys)
	var out []byte
	for _, key := range keys {
		out = append(out, byKey[key]...)
	}
	return out
}

func sha256Hex(parts ...[]byte) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write(part)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
